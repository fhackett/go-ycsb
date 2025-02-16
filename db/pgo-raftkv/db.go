package pgo_raftkv

import (
	"context"
	"database/sql"
	"encoding/json"
	"example.org/raftkvs"
	"fmt"
	"github.com/UBC-NSS/pgo/distsys"
	"github.com/UBC-NSS/pgo/distsys/resources"
	"github.com/UBC-NSS/pgo/distsys/tla"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	"go.uber.org/multierr"
	"strings"
	"time"
)

func assert(cond bool) {
	if !cond {
		panic("data integrity failed!")
	}
}

type raftClient struct {
	endpoints         []string
	endpointMonitors  map[string]string
	clientReplyPoints []string
	requestTimeout    time.Duration
	useInts           bool

	clientThreads []*raftClientThread
}

type threadIdxTag struct{}

type raftClientThread struct {
	clientCtx              *distsys.MPCalContext
	errCh                  chan error
	inCh, outCh, timeoutCh chan tla.TLAValue
}

func (cfg *raftClient) ToSqlDB() *sql.DB {
	return nil
}

func (cfg *raftClient) Close() error {
	var err error
	for _, client := range cfg.clientThreads {
		client.clientCtx.Stop()
		err = multierr.Append(err, <-client.errCh)
	}
	if err != nil {
		fmt.Printf("error closing RaftKV clients %v\n", err)
	}
	return err
}

func (cfg *raftClient) InitThread(ctx context.Context, threadIdx int, threadCount int) context.Context {
	if threadCount != len(cfg.clientReplyPoints) {
		panic(fmt.Errorf("%s must contain %d elements (equal to thread count); contains %v", pgoRaftKVClientReplyPoints, threadCount, cfg.clientReplyPoints))
	}

	errCh := make(chan error, 1)
	numServers := len(cfg.endpoints)
	constants := []distsys.MPCalContextConfigFn{
		distsys.DefineConstantValue("NumServers", tla.MakeTLANumber(int32(numServers))),
		distsys.DefineConstantValue("ExploreFail", tla.TLA_FALSE),
		distsys.DefineConstantValue("KeySet", tla.MakeTLASet()), // at runtime, we support growing the key set
		distsys.DefineConstantValue("Debug", tla.TLA_FALSE),
	}
	self := tla.MakeTLAString(cfg.clientReplyPoints[threadIdx])
	inChan := make(chan tla.TLAValue)
	outChan := make(chan tla.TLAValue)
	timeoutCh := make(chan tla.TLAValue, 1)
	clientCtx := distsys.NewMPCalContext(self, raftkvs.AClient,
		distsys.EnsureMPCalContextConfigs(constants...),
		distsys.EnsureArchetypeRefParam("net", resources.RelaxedMailboxesMaker(func(idx tla.TLAValue) (resources.MailboxKind, string) {
			if idx.Equal(self) {
				return resources.MailboxesLocal, idx.AsString()
			} else if idx.IsNumber() && int(idx.AsNumber()) <= len(cfg.endpoints) {
				return resources.MailboxesRemote, cfg.endpoints[int(idx.AsNumber())-1]
			} else if idx.IsString() {
				return resources.MailboxesRemote, idx.AsString()
			} else {
				panic(fmt.Errorf("count not link index to hostname: %v", idx))
			}
		})),
		distsys.EnsureArchetypeRefParam("fd", resources.FailureDetectorMaker(
			func(index tla.TLAValue) string {
				endpoint := cfg.endpoints[index.AsNumber()-1]
				monAddr, ok := cfg.endpointMonitors[endpoint]
				if !ok {
					panic(fmt.Errorf("%v is not a server whose monitor we know! options: %v", index, cfg.endpointMonitors))
				}
				return monAddr
			},
			resources.WithFailureDetectorPullInterval(100*time.Millisecond),
			resources.WithFailureDetectorTimeout(200*time.Millisecond),
		)),
		distsys.EnsureArchetypeRefParam("in", resources.InputChannelMaker(inChan)),
		distsys.EnsureArchetypeRefParam("out", resources.OutputChannelMaker(outChan)),
		distsys.EnsureArchetypeDerivedRefParam("netLen", "net", resources.MailboxesLengthMaker),
		distsys.EnsureArchetypeRefParam("timeout", resources.InputChannelMaker(timeoutCh)))

	clientThread := &raftClientThread{
		clientCtx: clientCtx,
		errCh:     errCh,
		inCh:      inChan,
		outCh:     outChan,
		timeoutCh: timeoutCh,
	}

	cfg.clientThreads = append(cfg.clientThreads, clientThread)
	if len(cfg.clientThreads) > threadCount {
		panic("too many client threads!")
	}

	go func() {
		errCh <- clientCtx.Run()
	}()

	return context.WithValue(ctx, threadIdxTag{}, clientThread)
}

func (cfg *raftClient) CleanupThread(_ context.Context) {
	// leave cleanup to Close() API
}

func (cfg *raftClient) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	client := ctx.Value(threadIdxTag{}).(*raftClientThread)
	keyStr := table + "/" + key

	var fieldFilter map[string]bool = nil
	if len(fields) != 0 {
		fieldFilter = make(map[string]bool)
		for _, field := range fields {
			fieldFilter[field] = true
		}
	}
	client.inCh <- tla.MakeTLARecord([]tla.TLARecordField{
		{Key: tla.MakeTLAString("type"), Value: raftkvs.Get(client.clientCtx.IFace())},
		{Key: tla.MakeTLAString("key"), Value: tla.MakeTLAString(keyStr)},
	})

	for {
		select {
		case resp := <-client.outCh:
			//log.Printf("[get] %s received %v", client.clientCtx.IFace().Self().AsString(), resp)
			assert(resp.ApplyFunction(tla.MakeTLAString("msuccess")).AsBool())
			typ := resp.ApplyFunction(tla.MakeTLAString("mtype"))
			mresp := resp.ApplyFunction(tla.MakeTLAString("mresponse"))
			respKey := mresp.ApplyFunction(tla.MakeTLAString("key")).AsString()
			assert(typ.Equal(raftkvs.ClientGetResponse(client.clientCtx.IFace())))
			assert(respKey == keyStr)

			if !mresp.ApplyFunction(tla.MakeTLAString("ok")).AsBool() {
				return nil, fmt.Errorf("key not found: %s", keyStr)
			}

			if cfg.useInts {
				// short-circuit attempting to parse the result, it's a random int
				return make(map[string][]byte), nil
			}
			result := make(map[string][]byte)
			it := mresp.ApplyFunction(tla.MakeTLAString("value")).AsFunction().Iterator()
			for !it.Done() {
				k, v := it.Next()
				kStr := k.(tla.TLAValue).AsString()
				if fieldFilter == nil || fieldFilter[kStr] {
					result[kStr] = []byte(v.(tla.TLAValue).AsString())
				}
			}
			return result, nil
		case <-time.After(cfg.requestTimeout):
			// clear timeout channel
			select {
			case <-client.timeoutCh:
			default:
			}
			client.timeoutCh <- tla.TLA_TRUE
		}
	}
}

func (cfg *raftClient) Scan(_ context.Context, _ string, _ string, _ int, _ []string) ([]map[string][]byte, error) {
	return nil, fmt.Errorf("pgo-raftkv does not implement key scan")
}

func (cfg *raftClient) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	result, err := cfg.Read(ctx, table, key, nil)
	if err != nil {
		return err
	}
	for k := range values {
		result[k] = values[k]
	}
	return cfg.Insert(ctx, table, key, result)
}

func (cfg *raftClient) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	client := ctx.Value(threadIdxTag{}).(*raftClientThread)
	keyStr := table + "/" + key

	kvFn := func() tla.TLAValue {
		if cfg.useInts {
			valuesBytes, err := json.Marshal(&values)
			if err != nil {
				panic(err)
			}
			return tla.MakeTLAString(fmt.Sprintf("%d", len(valuesBytes)))
		}
		var kvPairs []tla.TLARecordField
		for k := range values {
			kvPairs = append(kvPairs, tla.TLARecordField{
				Key:   tla.MakeTLAString(k),
				Value: tla.MakeTLAString(string(values[k])),
			})
		}
		return tla.MakeTLARecord(kvPairs)
	}()
	client.inCh <- tla.MakeTLARecord([]tla.TLARecordField{
		{Key: tla.MakeTLAString("type"), Value: raftkvs.Put(client.clientCtx.IFace())},
		{Key: tla.MakeTLAString("key"), Value: tla.MakeTLAString(keyStr)},
		{Key: tla.MakeTLAString("value"), Value: kvFn},
	})

	for {
		select {
		case resp := <-client.outCh:
			//log.Printf("[put] %s received %v", client.clientCtx.IFace().Self().AsString(), resp)
			assert(resp.ApplyFunction(tla.MakeTLAString("msuccess")).AsBool())
			typ := resp.ApplyFunction(tla.MakeTLAString("mtype"))
			mresp := resp.ApplyFunction(tla.MakeTLAString("mresponse"))
			respKey := mresp.ApplyFunction(tla.MakeTLAString("key")).AsString()
			assert(typ.Equal(raftkvs.ClientPutResponse(client.clientCtx.IFace())))
			assert(respKey == keyStr)
			assert(mresp.ApplyFunction(tla.MakeTLAString("value")).Equal(kvFn))
			return nil
		case <-time.After(cfg.requestTimeout):
			// clear timeout channel
			select {
			case <-client.timeoutCh:
			default:
			}
			client.timeoutCh <- tla.TLA_TRUE
		}
	}
}

func (cfg *raftClient) Delete(ctx context.Context, table string, key string) error {
	return cfg.Insert(ctx, table, key, make(map[string][]byte))
}

const (
	pgoRaftKVEndpoints         = "pgo-raftkv.endpoints"
	pgoRaftKVEndpointMonitors  = "pgo-raftkv.endpointmonitors"
	pgoRaftKVClientReplyPoints = "pgo-raftkv.clientreplypoints"
	pgoRaftKVRequestTimeout    = "pgo-raftkv.requesttimeout"
	pgoRaftKVUseInts           = "ycsb.useints"
)

type raftCreator struct{}

func (_ raftCreator) Create(props *properties.Properties) (ycsb.DB, error) {
	endpoints, ok := props.Get(pgoRaftKVEndpoints)
	if !ok {
		return nil, fmt.Errorf("must specify %s", pgoRaftKVEndpoints)
	}

	endpointMonitors, ok := props.Get(pgoRaftKVEndpointMonitors)
	if !ok {
		return nil, fmt.Errorf("must specify %s", pgoRaftKVEndpointMonitors)
	}
	endPointMonitorMap := make(map[string]string)
	for _, pairStr := range strings.Split(endpointMonitors, ",") {
		pair := strings.Split(pairStr, "->")
		if len(pair) != 2 {
			return nil, fmt.Errorf("count not parse mapping %s in %s; expecting endpoint:mport->monitor:mport", pairStr, pgoRaftKVEndpointMonitors)
		}
		endPointMonitorMap[pair[0]] = pair[1]
	}

	clientReplyPoints, ok := props.Get(pgoRaftKVClientReplyPoints)
	if !ok {
		return nil, fmt.Errorf("must specify %s", pgoRaftKVClientReplyPoints)
	}

	return &raftClient{
		endpoints:         strings.Split(endpoints, ","),
		endpointMonitors:  endPointMonitorMap,
		clientReplyPoints: strings.Split(clientReplyPoints, ","),
		requestTimeout:    props.GetParsedDuration(pgoRaftKVRequestTimeout, time.Second*1),
		useInts:           props.GetBool(pgoRaftKVUseInts, false),
	}, nil
}

func init() {
	ycsb.RegisterDBCreator("pgo-raftkv", raftCreator{})
}
