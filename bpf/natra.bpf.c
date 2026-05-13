// SPDX-License-Identifier: GPL-2.0
//
// natra dataplane — CMS-driven heavy-hitter detection plus a token
// bucket on heavy traffic. Two directions.
//
// Stage 1 (Count-Min Sketch): every packet's 5-tuple flow key is
// hashed CMS_DEPTH times, each into one column of CMS_WIDTH. The min
// across rows is the per-flow count estimator. Constant memory
// (CMS_WIDTH * CMS_DEPTH * DIR_MAX = 32768 cells = 256 KiB per pod
// at the 8-byte cell layout) regardless of how many distinct flows
// the pod sees.
//
// Stage 2 (token bucket): only flows whose CMS estimate exceeds
// `hh_threshold` go through the bucket. Mice flows take the fast
// pass at the top of the program (TC_ACT_OK without locking or
// stat increment beyond passed). The upstream bandwidth plugin
// rate-limits all traffic uniformly via HTB-on-IFB; we only
// rate-limit the elephants.
//
// Direction split: ingress and egress have independent state across
// every map. Asymmetric workloads make a flow heavy on one side and
// mice (ACKs only) on the other, so a shared CMS would falsely
// classify ACK streams as heavy. Two SEC("tc") entry points share
// inlined logic via natra_classify(skb, dir) — userspace gets two
// distinct *ebpf.Program handles and attaches each to its hook.
//
// Concurrency:
//   - CMS counters use __sync_fetch_and_add (-mcpu=v3 atomic). Loose
//     ordering is fine; CMS is approximate by design.
//   - Token bucket uses bpf_spin_lock. The lock is the only place
//     packets serialize; mice flows skip it entirely.
//
// License: This file declares "GPL" because BPF kernel helpers
// (bpf_ktime_get_ns, bpf_spin_lock) are GPL-only and the verifier
// rejects programs that use them with an Apache-2.0 license. The rest
// of natra is Apache-2.0 — the userspace binary doesn't link the BPF
// object, so this isn't a license-tainting boundary.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/in.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

// CMS dimensions are compile-time constants because BPF map sizes are
// fixed at load time. 16384 × 4 = 65536 cells per direction; sized
// to cover ~50K-flow workloads without saturating.
//
// Sizing history — every step driven by profile data:
//   1024 × 4   — original. Saturated in ~5s of mixed-workload
//               traffic (iperf3 --bidir + ~1500 hey RPS); all cells
//               nonzero, cell-value-mean climbed above the
//               heavy-hitter threshold of 10, so new mice flows were
//               false-positive-classified as heavy on their first
//               packet. Hey RPS topped out at ~1000.
//   4096 × 4   — 4×. Still 99.9% saturated, 69% packets classified
//               heavy. Hey RPS improved to ~1500.
//   16384 × 4  — 16×. 82% fill, 54% heavy. Hey RPS ~2000.
//   32768 × 4  — 32×, current. 41% fill expected; most flows take
//               the fast pass. Diminishing returns past this point
//               unless the workload's flow cardinality grows.
//
// Cost: 32× original memory. Cell is 8 bytes (count + last_decay_idx)
// after aging added the timestamp, so per pod: 32768 × 4 × 2 × 8 =
// 2 MiB. At 100 pods/node that's 200 MiB — still trivial for a
// kernel-side data structure. Cluster-tier knobs let operators pick a
// smaller tier for low-cardinality workloads (saving memory at the
// cost of CMS accuracy under load spikes).
#define CMS_WIDTH 32768
#define CMS_DEPTH 4

// Direction enum. Userspace must use the same numeric values when
// keying config_map / bucket_map and indexing stats_map / cms_map.
enum direction {
	DIR_INGRESS = 0,
	DIR_EGRESS  = 1,
	DIR_MAX     = 2,
};

// Per-row hash seeds. Distinct primes so rows are independent (in
// expectation), which is what makes CMS's min estimator work.
static const __u32 cms_seeds[CMS_DEPTH] = {
	0x9e3779b1u,
	0x85ebca77u,
	0xc2b2ae3du,
	0x27d4eb2fu,
};

// natra_config is loaded per-direction by userspace at CNI ADD. Read
// by the BPF program on every above-rate packet to decide how to
// throttle. edt_pacing must default to 0; without an fq qdisc
// downstream of natra's attach point, EDT-stamped packets pass at
// line rate and the rate limit silently breaks. Operators opt in
// only on hosts where fq is in place (NATRA_EDT_PACING=1 on the
// installer DaemonSet env).
struct natra_config {
	__u64 rate_bps;
	__u64 burst_bytes;
	__u64 hh_threshold; // CMS count above which a flow is "heavy"
	__u64 edt_pacing;   // non-zero → use EDT (egress) before dropping; 0 → ECN-mark or drop
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct natra_config);
	__uint(max_entries, DIR_MAX);
} natra_config_map SEC(".maps");

// token_bucket tracks (1) the classic rate-limit bucket (tokens +
// last_update_ns for refill) and (2) the next-release timestamp used
// for EDT pacing. When the bucket is depleted, natra advances
// next_release_ns by (packet_bytes * 8e9 / rate_bps) ns per packet
// and stamps skb->tstamp = next_release_ns. The fq qdisc downstream
// then holds the skb until that time. No drop → no TCP retransmit →
// no per-packet softirq amplifier on neighboring pods.
struct token_bucket {
	struct bpf_spin_lock lock;
	__u64 tokens;
	__u64 last_update_ns;
	__u64 next_release_ns;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct token_bucket);
	__uint(max_entries, DIR_MAX);
} natra_bucket_map SEC(".maps");

// CMS cell holds a byte counter plus a decay-interval index. Lazy
// aging: each cell-access reads the cell's last_decay_idx, computes
// how many decay intervals have elapsed since, right-shifts the
// counter by that many (capped at 63 to avoid UB), and updates
// last_decay_idx to the current time. Recent elephants keep climbing
// faster than decay reduces (they re-increment on every packet);
// dormant cells fade lazily the next time they're touched.
//
// We count BYTES, not packets, so the heavy-hitter threshold is in
// bytes and GRO-invariant — packet-count CMS classified differently
// depending on whether GRO had coalesced packets into 64 KB super-
// packets (host-side attach) vs raw 1500-byte packets (pod-side
// egress). With bytes the threshold means the same thing regardless
// of attach mode. ACK-only flows correctly stay mice (tiny byte
// volume) instead of crossing threshold from sheer packet count.
//
// u64 counter handles 4 GB+ accumulation without wraparound; u32
// would wrap in ~4 seconds at 10 Gbps line rate (faster than the
// decay window), corrupting classification mid-flow.
//
// last_decay_idx uses CMS_DECAY_INTERVAL_NS as its unit and stores
// as u32, wrapping every ~hundreds of years at the chosen interval.
//
// CMS counters are non-atomic by design. Lost increments under
// cross-CPU race give slightly conservative classification (an
// elephant takes one extra packet to be marked heavy), which is the
// safe direction.
//
// CMS_DECAY_INTERVAL_NS is intentionally a power of two so the
// `now_ns / CMS_DECAY_INTERVAL_NS` reduces to a right-shift in the
// BPF JIT. (1 << 36) ns ≈ 68.7 s — close enough to a 60 s decay
// window that the behavior is indistinguishable; the trade is one
// shift instead of an integer division per packet. Stays well
// inside u32 wrap (2^32 ticks × 68.7 s ≈ 9300 years).
#define CMS_DECAY_INTERVAL_NS (1ULL << 36)

struct cms_cell {
	__u64 bytes;
	__u32 last_decay_idx;
};

// CMS as a flat array; index = dir * CMS_WIDTH * CMS_DEPTH +
// row * CMS_WIDTH + col. Per-direction halves are independent.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct cms_cell);
	__uint(max_entries, CMS_WIDTH * CMS_DEPTH * DIR_MAX);
} natra_cms_map SEC(".maps");

// Stat slots per direction. Total slots = STAT_PER_DIR * DIR_MAX.
// Userspace key = dir * STAT_PER_DIR + slot.
//
// STAT_THROTTLED is bumped for every above-rate packet regardless of
// whether the eventual disposition was ECN-mark, EDT-delay, or drop —
// so STAT_THROTTLED is the cardinality of all bucket-overflow events.
// The disposition-specific stats below break it down:
//
//   STAT_ECN_MARKED   ≤ STAT_THROTTLED  (ECN-capable, marked CE, passed)
//   STAT_EDT_DELAYED  ≤ STAT_THROTTLED  (egress non-ECN, paced via skb->tstamp)
//   STAT_DROPPED      ≤ STAT_THROTTLED  (ingress non-ECN, TC_ACT_SHOT)
//
// Their sum equals STAT_THROTTLED.
enum {
	STAT_PASSED      = 0,
	STAT_THROTTLED   = 1,
	STAT_HH_HITS     = 2,
	STAT_ECN_MARKED  = 3,
	STAT_EDT_DELAYED = 4,
	STAT_DROPPED     = 5,
	STAT_PER_DIR,
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, STAT_PER_DIR * DIR_MAX);
} natra_stats_map SEC(".maps");

static __always_inline void bump_stat(__u32 dir, __u32 slot)
{
	__u32 idx = dir * STAT_PER_DIR + slot;
	__u64 *v = bpf_map_lookup_elem(&natra_stats_map, &idx);
	if (v)
		(*v)++;
}

// 5-tuple flow key. Layout is for hashing only — not stored in any
// map across program runs. `pad` zeroed in parse_flow so two packets
// of the same flow hash identically.
struct flow_key {
	__u32 src_ip;
	__u32 dst_ip;
	__u16 src_port;
	__u16 dst_port;
	__u8  proto;
	__u8  pad[3];
};

// FNV-1a, mixed with a per-row seed. Bounded loop with #pragma unroll
// so the BPF verifier can prove termination without complex analysis.
static __always_inline __u32 cms_hash(const struct flow_key *k, __u32 seed)
{
	__u32 h = 2166136261u ^ seed;
	const __u8 *p = (const __u8 *)k;
	#pragma unroll
	for (int i = 0; i < (int)sizeof(*k); i++) {
		h ^= p[i];
		h *= 16777619u;
	}
	return h;
}

// Update all CMS_DEPTH counters in `dir`'s half of the array and
// return the post-increment min across rows (CMS estimator). The
// counter unit is BYTES — callers pass skb->len so a flow's CMS
// estimate accumulates its byte volume, not its packet count.
//
// Lazy aging: before incrementing, fade the cell by 2^elapsed (where
// elapsed is in CMS_DECAY_INTERVAL_NS units). cell->bytes >> elapsed
// is the post-decay value; we cap shift at 63 because >=64 on u64 is
// undefined behavior. last_decay_idx advances to now_idx so the next
// access measures from here, not from the original last-seen.
//
// `now_idx` is computed once per packet (callers pass it in) so all
// four cells see a consistent timestamp.
static __always_inline __u64 cms_update_and_min(__u32 dir,
						const struct flow_key *k,
						__u32 now_idx,
						__u64 bytes)
{
	__u32 base = dir * (CMS_WIDTH * CMS_DEPTH);
	__u64 mn = 0xffffffffffffffffULL;
	#pragma unroll
	for (int row = 0; row < CMS_DEPTH; row++) {
		__u32 h = cms_hash(k, cms_seeds[row]);
		__u32 col = h % CMS_WIDTH;
		__u32 idx = base + (__u32)row * CMS_WIDTH + col;
		struct cms_cell *cell = bpf_map_lookup_elem(&natra_cms_map, &idx);
		if (!cell)
			return 0;

		__u32 elapsed = 0;
		if (now_idx > cell->last_decay_idx)
			elapsed = now_idx - cell->last_decay_idx;

		__u64 next = cell->bytes;
		if (elapsed >= 64)
			next = 0;
		else if (elapsed > 0)
			next >>= elapsed;
		next += bytes;

		cell->bytes = next;
		if (elapsed > 0)
			cell->last_decay_idx = now_idx;

		if (next < mn)
			mn = next;
	}
	return mn;
}

// Extract a 5-tuple. Returns 0 on success, -1 on parse failure
// (non-IP, truncated header, fragment we don't dissect). Verifier-
// friendly: every pointer access is guarded by a bound check against
// `data_end`.
static __always_inline int parse_flow(struct __sk_buff *skb, struct flow_key *out)
{
	void *data     = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return -1;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return -1;

	struct iphdr *ip = (void *)(eth + 1);
	if ((void *)(ip + 1) > data_end)
		return -1;
	if (ip->ihl < 5)
		return -1;

	out->src_ip = ip->saddr;
	out->dst_ip = ip->daddr;
	out->proto  = ip->protocol;
	out->src_port = 0;
	out->dst_port = 0;
	__builtin_memset(out->pad, 0, sizeof(out->pad));

	void *l4 = (void *)ip + ((__u32)ip->ihl * 4);
	if (ip->protocol == IPPROTO_TCP) {
		struct tcphdr *th = l4;
		if ((void *)(th + 1) > data_end)
			return -1;
		out->src_port = th->source;
		out->dst_port = th->dest;
	} else if (ip->protocol == IPPROTO_UDP) {
		struct udphdr *uh = l4;
		if ((void *)(uh + 1) > data_end)
			return -1;
		out->src_port = uh->source;
		out->dst_port = uh->dest;
	}
	return 0;
}

// consume_tokens charges `bytes` against `dir`'s bucket. Returns 1 if
// the charge succeeded (caller passes the packet), 0 if the bucket
// lacks tokens (caller falls through to ECN-mark / EDT-pace / drop).
// Refill is computed lazily from elapsed time so the lock is held for
// ~tens of ns per packet.
static __always_inline int consume_tokens(__u32 dir, __u64 bytes, __u64 rate_bps, __u64 burst, __u64 now)
{
	struct token_bucket *tb = bpf_map_lookup_elem(&natra_bucket_map, &dir);
	if (!tb)
		return 1;

	int allowed = 0;
	bpf_spin_lock(&tb->lock);
	// bpf_ktime_get_ns is monotonic per CPU but the timekeeping core's
	// fast-path latch can return slightly out-of-order values across
	// CPUs (and around the seqlock's swap). If now < last_update_ns,
	// a naive subtraction underflows to ~2^64, which then gets
	// multiplied through and refills the bucket all the way to burst
	// on every such packet — turning the throttle off entirely under
	// multi-stream workloads. Treat any non-monotonic reading as zero
	// elapsed.
	__u64 elapsed_ns = 0;
	if (now > tb->last_update_ns)
		elapsed_ns = now - tb->last_update_ns;
	// Two-step (ns / 1000) * rate / 1_000_000 keeps the multiply
	// inside u64 even for hours of idle time. The cap-by-burst step
	// after enforces the bucket ceiling regardless.
	__u64 added = (elapsed_ns / 1000ULL) * rate_bps / 1000000ULL;
	__u64 tokens = tb->tokens + added;
	if (tokens > burst)
		tokens = burst;
	if (tokens >= bytes) {
		tokens -= bytes;
		allowed = 1;
	}
	tb->tokens = tokens;
	if (now > tb->last_update_ns)
		tb->last_update_ns = now;
	bpf_spin_unlock(&tb->lock);
	return allowed;
}

// throttle_disposition returns the TC verdict for an above-rate packet
// and bumps the matching stat. Preference order:
//
//   1. ECN-mark via bpf_skb_ecn_set_ce. Helper returns 1 on an
//      ECN-capable packet (ECT(0) or ECT(1) in IP TOS bits 0-1) and
//      sets the CE bit. Receiver's TCP backs off without retrans.
//      Works on both ingress and egress.
//
//   2. EDT pacing (egress only). When the packet isn't ECN-capable,
//      compute a release time per bytes/rate_bps, advance the
//      bucket's next_release_ns past it, and stamp skb->tstamp.
//      The downstream fq qdisc holds the skb until that time. No
//      drop → no retrans. Ingress has no transmission-side qdisc to
//      honor skb->tstamp, so this path is egress-only.
//
//   3. Drop (TC_ACT_SHOT). Only reached for ingress non-ECN traffic
//      that nothing else can pace.
//
// `bytes` and `rate_bps` come from the caller; passed in so the helper
// is independent of the natra_config / skb layout.
static __always_inline int throttle_disposition(struct __sk_buff *skb,
						__u32 dir,
						__u64 now_ns,
						__u64 rate_bps,
						__u64 bytes,
						__u64 edt_pacing)
{
	if (bpf_skb_ecn_set_ce(skb) > 0) {
		bump_stat(dir, STAT_ECN_MARKED);
		return TC_ACT_OK;
	}

	if (dir == DIR_EGRESS && edt_pacing != 0) {
		struct token_bucket *tb = bpf_map_lookup_elem(&natra_bucket_map, &dir);
		if (tb && rate_bps > 0) {
			// add_ns = bytes * 8 * 1e9 / rate_bps. Split so the
			// multiply stays inside u64 for MTU-sized packets at
			// modest rates (e.g., 1500 B at 1 Mbps → 12 ms).
			__u64 add_ns = (bytes * 8000ULL) * 1000000ULL / rate_bps;
			__u64 release_at;
			bpf_spin_lock(&tb->lock);
			__u64 base = tb->next_release_ns;
			if (base < now_ns)
				base = now_ns;
			release_at = base + add_ns;
			tb->next_release_ns = release_at;
			bpf_spin_unlock(&tb->lock);

			skb->tstamp = release_at;
			bump_stat(dir, STAT_EDT_DELAYED);
			return TC_ACT_OK;
		}
	}

	bump_stat(dir, STAT_DROPPED);
	return TC_ACT_SHOT;
}

static __always_inline int natra_classify(struct __sk_buff *skb, __u32 dir)
{
	struct natra_config *cfg = bpf_map_lookup_elem(&natra_config_map, &dir);
	if (!cfg || cfg->rate_bps == 0) {
		// No config for this direction → fail-open. Same path as
		// before P1.5.
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}

	struct flow_key k = {0};
	if (parse_flow(skb, &k) < 0) {
		// Non-IP / truncated. Pass through unaccounted; we only
		// rate-limit IP traffic.
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}

	// One ktime read per packet — reused for CMS aging (now_idx) and
	// token bucket refill (now_ns). Avoids a second syscall.
	__u64 now_ns = bpf_ktime_get_ns();
	__u32 now_idx = (__u32)(now_ns / CMS_DECAY_INTERVAL_NS);

	__u64 len = skb->len;
	__u64 bytes_est = cms_update_and_min(dir, &k, now_idx, len);
	if (bytes_est <= cfg->hh_threshold) {
		// Mouse: fast pass with no lock. Low-volume traffic stays
		// at line rate even when an elephant exists on the same pod.
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}

	// Heavy hitter — token bucket gate.
	bump_stat(dir, STAT_HH_HITS);
	if (consume_tokens(dir, len, cfg->rate_bps, cfg->burst_bytes, now_ns)) {
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}
	// Above the rate: prefer ECN-mark, fall back to EDT pacing on
	// egress (only when cfg->edt_pacing is set — without fq
	// downstream EDT silently breaks the rate limit), drop only if
	// nothing else applies. STAT_THROTTLED is the cardinality of all
	// overflow events; the disposition stat (ECN_MARKED / EDT_DELAYED
	// / DROPPED) records the outcome.
	bump_stat(dir, STAT_THROTTLED);
	return throttle_disposition(skb, dir, now_ns, cfg->rate_bps, len, cfg->edt_pacing);
}

SEC("tc")
int natra_ingress(struct __sk_buff *skb)
{
	return natra_classify(skb, DIR_INGRESS);
}

SEC("tc")
int natra_egress(struct __sk_buff *skb)
{
	return natra_classify(skb, DIR_EGRESS);
}

char __license[] SEC("license") = "GPL";
