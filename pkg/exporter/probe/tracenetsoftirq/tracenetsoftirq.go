package tracenetsoftirq

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"github.com/alibaba/kubeskoop/pkg/exporter/bpfutil"
	"github.com/alibaba/kubeskoop/pkg/exporter/proto"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/exp/slog"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -cflags $BPF_CFLAGS -type insp_softirq_event_t bpf ../../../../bpf/net_softirq.c -- -I../../../../bpf/headers -D__TARGET_ARCH_x86
const (
	NETSOFTIRQ_SCHED_SLOW   = "net_softirq_schedslow"       //nolint
	NETSOFTIRQ_SCHED_100MS  = "net_softirq_schedslow100ms"  //nolint
	NETSOFTIRQ_EXCUTE_SLOW  = "net_softirq_excuteslow"      //nolint
	NETSOFTIRQ_EXCUTE_100MS = "net_softirq_excuteslow100ms" //nolint
)

var (
	ModuleName = "insp_netsoftirq" // nolint
	probe      = &NetSoftirqProbe{once: sync.Once{}, mtx: sync.Mutex{}}
	objs       = bpfObjects{}
	links      = []link.Link{}
	metricsMap = map[string]map[uint32]uint64{}

	events  = []string{"NETSOFTIRQ_SCHED_SLOW", "NETSOFTIRQ_SCHED_100MS", "NETSOFTIRQ_EXCUTE_SLOW", "NETSOFTIRQ_EXCUTE_100MS"}
	metrics = []string{NETSOFTIRQ_SCHED_SLOW, NETSOFTIRQ_SCHED_100MS, NETSOFTIRQ_EXCUTE_SLOW, NETSOFTIRQ_EXCUTE_100MS}
)

func GetProbe() *NetSoftirqProbe {
	return probe
}

func init() {
	for m := range metrics {
		metricsMap[metrics[m]] = map[uint32]uint64{
			0: 0,
		}
	}
}

type NetSoftirqProbe struct {
	enable bool
	sub    chan<- proto.RawEvent
	once   sync.Once
	mtx    sync.Mutex
}

func (p *NetSoftirqProbe) Name() string {
	return ModuleName
}

func (p *NetSoftirqProbe) Ready() bool {
	return p.enable
}

func (p *NetSoftirqProbe) GetEventNames() []string {
	return events
}

func (p *NetSoftirqProbe) GetMetricNames() []string {
	return metrics
}

func (p *NetSoftirqProbe) Collect(_ context.Context) (map[string]map[uint32]uint64, error) {

	return metricsMap, nil
}

func (p *NetSoftirqProbe) Close() error {
	if p.enable {
		for _, link := range links {
			link.Close()
		}
		links = []link.Link{}
	}

	return nil
}

func (p *NetSoftirqProbe) Start(ctx context.Context) {
	if p.enable {
		return
	}

	p.once.Do(func() {
		err := loadSync()
		if err != nil {
			slog.Ctx(ctx).Warn("start", "module", ModuleName, "err", err)
			return
		}
		p.enable = true
	})

	if !p.enable {
		// if load failed, do not start process
		return
	}

	reader, err := perf.NewReader(objs.bpfMaps.InspSoftirqEvents, int(unsafe.Sizeof(bpfInspSoftirqEventT{})))
	if err != nil {
		slog.Ctx(ctx).Warn("start new perf reader", "module", ModuleName, "err", err)
		return
	}

	for {
		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				slog.Ctx(ctx).Info("received signal, exiting..", "module", ModuleName)
				return
			}
			slog.Ctx(ctx).Info("reading from reader", "module", ModuleName, "err", err)
			continue
		}

		if record.LostSamples != 0 {
			slog.Ctx(ctx).Info("Perf event ring buffer full", "module", ModuleName, "drop samples", record.LostSamples)
			continue
		}

		var event bpfInspSoftirqEventT
		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			slog.Ctx(ctx).Info("parsing event", "module", ModuleName, "err", err)
			continue
		}

		rawevt := proto.RawEvent{
			Netns: 0,
		}

		/*
					#define PHASE_SCHED 1
			        #define PHASE_EXCUTE 2
		*/
		switch event.Phase {
		case 1:
			if event.Latency > 100000000 {
				rawevt.EventType = "NETSOFTIRQ_SCHED_100MS"
				p.updateMetrics(NETSOFTIRQ_SCHED_100MS)
			} else {
				rawevt.EventType = "NETSOFTIRQ_SCHED_SLOW"
				p.updateMetrics(NETSOFTIRQ_SCHED_SLOW)
			}
		case 2:
			if event.Latency > 100000000 {
				rawevt.EventType = "NETSOFTIRQ_EXCUTE_100MS"
				p.updateMetrics(NETSOFTIRQ_EXCUTE_100MS)
			} else {
				rawevt.EventType = "NETSOFTIRQ_EXCUTE_SLOW"
				p.updateMetrics(NETSOFTIRQ_EXCUTE_SLOW)
			}

		default:
			slog.Ctx(ctx).Info("parsing event", "module", ModuleName, "ignore", event)
			continue
		}

		rawevt.EventBody = fmt.Sprintf("cpu=%d pid=%d latency=%s ", event.Cpu, event.Pid, bpfutil.GetHumanTimes(event.Latency))
		if p.sub != nil {
			slog.Ctx(ctx).Debug("broadcast event", "module", ModuleName)
			p.sub <- rawevt
		}
	}
}

func (p *NetSoftirqProbe) updateMetrics(k string) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if _, ok := metricsMap[k]; ok {
		metricsMap[k][0]++
	}
}

func (p *NetSoftirqProbe) Register(receiver chan<- proto.RawEvent) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	p.sub = receiver

	return nil
}

func loadSync() error {
	// 准备动作
	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	opts := ebpf.CollectionOptions{}
	// 获取btf信息
	opts.Programs = ebpf.ProgramOptions{
		KernelTypes: bpfutil.LoadBTFSpecOrNil(),
	}

	// 获取Loaded的程序/map的fd信息
	if err := loadBpfObjects(&objs, &opts); err != nil {
		if strings.Contains(err.Error(), "no BTF found for kernel") {
			_BpfBytes, err = bpfutil.CompileBPF("net_softirq")
			if err != nil {
				return err
			}
			opts.Programs.KernelTypes = nil
			err = loadBpfObjects(&objs, &opts)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("loading objects: %v", err)
		}
	}

	prograise, err := link.Tracepoint("irq", "softirq_raise", objs.TraceSoftirqRaise, &link.TracepointOptions{})
	if err != nil {
		return fmt.Errorf("link softirq_raise: %s", err.Error())
	}
	links = append(links, prograise)

	progentry, err := link.Tracepoint("irq", "softirq_entry", objs.TraceSoftirqEntry, &link.TracepointOptions{})
	if err != nil {
		return fmt.Errorf("link softirq_entry: %s", err.Error())
	}
	links = append(links, progentry)

	progexit, err := link.Tracepoint("irq", "softirq_exit", objs.TraceSoftirqExit, &link.TracepointOptions{})
	if err != nil {
		return fmt.Errorf("link softirq_exit: %s", err.Error())
	}
	links = append(links, progexit)

	return nil
}
