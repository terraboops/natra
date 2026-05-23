package perfrig

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runMixed is the bystander-aware workload: iperf3 --bidir against
// the annotated perf-server (elephant drains both buckets at once)
// plus two concurrent hey HTTP runs — one against perf-server
// (annotated mice; shares the elephant's bucket) and one against a
// bystander (unannotated; on the same worker node so it shares only
// the physical uplink). Reports annotated mice RPS / p50 / p99 (the
// CMS fast-pass story) and bystander RPS / p50 / p99 (collateral
// cost — natra must not charge an unannotated neighbor).
//
// Ports scripts/perf-vs-vanilla.sh's run_mixed_workload. The
// concurrency shape matters: --bidir keeps both buckets engaged
// while the mice run, surfacing per-direction state corruption
// that sequential ingress→egress→mice (in iperfSweep) misses.
const (
	mixedIperfDur       = 30
	mixedHeyDur         = 25
	mixedHeyLag         = 2
	mixedHeyConcurrency = 50
)

func (e *Executor) runMixed(ctx context.Context, phase Phase) (WorkloadReport, error) {
	wr := WorkloadReport{Kind: WorkloadMixed}
	ns := e.namespaceForPhase()
	kc := e.Substrate.KubeconfigPath()

	if err := e.deployBystander(ctx, ns, phase); err != nil {
		return wr, fmt.Errorf("deploy bystander: %w", err)
	}
	if err := kubectl(ctx, kc, nil, "wait", "--for=condition=Ready",
		"pod/bystander", "-n", ns, "--timeout=120s"); err != nil {
		return wr, fmt.Errorf("wait bystander: %w", err)
	}

	for s := 1; s <= e.Plan.Samples; s++ {
		e.logf("==> [%s] mixed sample %d/%d (iperf3 --bidir + hey × 2 concurrent)\n",
			phase, s, e.Plan.Samples)
		ms, err := e.runMixedSample(ctx, kc, ns, phase, s)
		if err != nil {
			return wr, fmt.Errorf("mixed sample %d: %w", s, err)
		}
		wr.MixedSamples = append(wr.MixedSamples, ms)
		e.logf("  [%s s%d] iperf ing=%.1fMbps eg=%.1fMbps | annotated mice %.0f rps p99=%.1fms | bystander %.0f rps p99=%.1fms\n",
			phase, s,
			ms.IperfIngressBps/1e6, ms.IperfEgressBps/1e6,
			ms.PodRPS, ms.PodP99,
			ms.BystanderRPS, ms.BystanderP99)
	}
	return wr, nil
}

// runMixedSample fires the elephant in the background, briefly
// lets it drain the bucket, then runs both hey clients in
// parallel. All three complete before this returns.
func (e *Executor) runMixedSample(ctx context.Context, kc, ns string, _ Phase, sample int) (MixedSample, error) {
	ms := MixedSample{Sample: sample}

	// Elephant: iperf3 --bidir against perf-server.
	iperfOut := make(chan string, 1)
	iperfErr := make(chan error, 1)
	go func() {
		out, err := captureKubectl(ctx, kc, "exec", "-n", ns, "perf-client", "-c", "tools", "--",
			"iperf3", "-c", "perf-server", "-t", strconv.Itoa(mixedIperfDur), "--bidir", "-J")
		iperfOut <- out
		iperfErr <- err
	}()

	// Brief lag so the elephant drains the bucket before mice start.
	time.Sleep(time.Duration(mixedHeyLag) * time.Second)

	// Two hey runs in parallel — one against annotated perf-server,
	// one against unannotated bystander on the same worker.
	type heyRet struct {
		out string
		err error
	}
	podHey, byHey := make(chan heyRet, 1), make(chan heyRet, 1)
	heyArgs := func(target string) []string {
		return []string{
			"exec", "-n", ns, "perf-client", "-c", "tools", "--",
			"hey", "-z", strconv.Itoa(mixedHeyDur) + "s", "-c", strconv.Itoa(mixedHeyConcurrency),
			"-disable-keepalive", "-o", "csv", "http://" + target + ":80/",
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out, err := captureKubectl(ctx, kc, heyArgs("perf-server")...)
		podHey <- heyRet{out, err}
	}()
	go func() {
		defer wg.Done()
		out, err := captureKubectl(ctx, kc, heyArgs("bystander")...)
		byHey <- heyRet{out, err}
	}()
	wg.Wait()

	// Collect: wait on the elephant too — it should have finished
	// shortly after the hey runs since mixedIperfDur > mixedHeyDur +
	// mixedHeyLag.
	iperfRaw := <-iperfOut
	if err := <-iperfErr; err != nil {
		return ms, fmt.Errorf("iperf3 --bidir: %w", err)
	}

	ing, eg, perr := parseIperfBidir([]byte(iperfRaw))
	if perr != nil {
		return ms, fmt.Errorf("parse iperf3 --bidir: %w", perr)
	}
	ms.IperfIngressBps = ing
	ms.IperfEgressBps = eg

	pod := <-podHey
	if pod.err != nil {
		return ms, fmt.Errorf("hey perf-server: %w", pod.err)
	}
	podR, perr := parseHeyCSV([]byte(pod.out), float64(mixedHeyDur))
	if perr != nil {
		return ms, fmt.Errorf("parse hey perf-server: %w", perr)
	}
	ms.PodRPS = podR.rps()
	ms.PodP50 = podR.P50 * 1000
	ms.PodP99 = podR.P99 * 1000

	by := <-byHey
	if by.err != nil {
		return ms, fmt.Errorf("hey bystander: %w", by.err)
	}
	byR, perr := parseHeyCSV([]byte(by.out), float64(mixedHeyDur))
	if perr != nil {
		return ms, fmt.Errorf("parse hey bystander: %w", perr)
	}
	ms.BystanderRPS = byR.rps()
	ms.BystanderP50 = byR.P50 * 1000
	ms.BystanderP99 = byR.P99 * 1000

	return ms, nil
}

// parseIperfBidir reads `iperf3 -J --bidir` output. In --bidir
// mode iperf3 reports two streams: streams[0] is the forward
// direction (client→server, the server's ingress) and streams[1]
// is the reverse (server→client, the server's egress). Each
// stream's sender.bits_per_second is the steady-state throughput
// for that direction.
func parseIperfBidir(raw []byte) (ingress, egress float64, err error) {
	var doc struct {
		End struct {
			Streams []struct {
				Sender struct {
					BitsPerSecond float64 `json:"bits_per_second"`
				} `json:"sender"`
			} `json:"streams"`
		} `json:"end"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, 0, err
	}
	if len(doc.End.Streams) < 2 {
		return 0, 0, fmt.Errorf("iperf3 --bidir: expected 2 streams, got %d", len(doc.End.Streams))
	}
	return doc.End.Streams[0].Sender.BitsPerSecond,
		doc.End.Streams[1].Sender.BitsPerSecond, nil
}

// deployBystander applies the bystander pod manifest pinned to
// the worker node. Only the Pod is needed (the Service is in the
// same multi-doc YAML; we keep it so hey can resolve `bystander`
// by name). Baseline phase strips the bandwidth annotations the
// same way perf-server does.
func (e *Executor) deployBystander(ctx context.Context, ns string, phase Phase) error {
	server, worker := e.Substrate.Nodes()
	manifest, err := renderPerfManifest(e.RepoRoot,
		"test/perf/realworld/bystander.yaml", ns, server, worker, e.PerfclientImage)
	if err != nil {
		return err
	}
	if phase == PhaseBaseline {
		manifest = stripBandwidthAnnotations(manifest)
	}
	return kubectl(ctx, e.Substrate.KubeconfigPath(),
		strings.NewReader(manifest), "apply", "-f", "-")
}
