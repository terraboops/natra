// SPDX-License-Identifier: GPL-2.0
//
// natra dataplane — CMS-driven heavy-hitter detection plus a token
// bucket on heavy traffic. Two directions.
//
// Stage 1 (Count-Min Sketch): every packet's 5-tuple flow key is
// hashed CMS_DEPTH times, each into one column of CMS_WIDTH. The min
// across rows is the per-flow count estimator. Constant memory
// (CMS_WIDTH * CMS_DEPTH * DIR_MAX = 262144 cells × 16 bytes per
// cell = 4 MiB per pod) regardless of how many distinct flows the
// pod sees.
//
// Stage 2 (token bucket): only flows whose CMS estimate exceeds
// `hh_threshold` go through the bucket. Mice flows take the fast
// pass at the top of the program (TC_ACT_OK without locking or
// stat increment beyond passed), up to a per-direction mouse-byte
// budget (natra_mice_map) so many small flows can't add up to an
// unthrottled elephant. The upstream bandwidth plugin
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
#include <linux/ipv6.h>
#include <linux/in.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

// CMS dimensions are compile-time constants because BPF map sizes are
// fixed at load time. 32768 × 4 = 131072 cells per direction;
// 262144 cells total per pod. Sized so saturation stays below ~50%
// for ~50K concurrent flows per direction — past that, collisions
// dominate and every flow looks heavy.
//
// Cell is 16 bytes (u64 bytes + u32 last_decay_idx, padded to
// 8-byte alignment), so per pod: 32768 × 4 × 2 × 16 = 4 MiB. At
// 100 pods/node that's 400 MiB — trivial for a kernel-side data
// structure.
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

// Aggregate cap on mouse fast-pass traffic. A flow stays a mouse until
// its CMS estimate crosses hh_threshold, so a pod that keeps opening
// new 5-tuples and sends just under the threshold on each would never
// reach the token bucket. Mouse bytes are summed per direction over a
// MICE_WINDOW_NS window; past the budget, further packets in that
// window go through the token bucket like heavy hitters.
//
// Budget = max(rate × 125 ms, MICE_BUDGET_HH × hh_threshold) per
// window. With the default threshold (rate × 100 ms) that's ~3× the
// configured rate of mouse traffic on top of the bucket — a bounded
// multiple instead of "whatever fits under the threshold per 5-tuple
// until the CMS saturates". Mice beyond it still pass whenever the
// bucket has tokens; they only lose the bypass while the pod is at
// its limit.
//
// The counter is a plain XADD (no fetch, so clsact on 5.x kernels
// still loads). Window rollover isn't atomic: a CPU racing the reset
// can lose a few increments, which errs toward passing mice for at
// most one window.
#define MICE_WINDOW_NS (1ULL << 27) // ≈134 ms; power of two → shift
#define MICE_BUDGET_HH 4ULL

struct mice_window {
	__u64 idx;
	__u64 bytes;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct mice_window);
	__uint(max_entries, DIR_MAX);
} natra_mice_map SEC(".maps");

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

// Max EDT-stamped delay before disposition falls through to ECN-mark
// or drop. Caps fq queue depth at MAX_EDT_DELAY_NS × rate (≈ 42
// MTU packets at 10 Mbps) so a sustained over-rate flow can't
// starve same-node neighbors of softirq time. 50 ms is generous
// against typical TCP retransmit timers (~200 ms) and well past
// intra-cluster RTT (sub-ms). See `throttle_disposition` and the
// `Bystander cost from EDT preservation` resolution in TODO_LINUX.md.
#define MAX_EDT_DELAY_NS 50000000ULL

// Cell layout: u64 + u32 = 12 bytes of fields, padded to 16 bytes
// for 8-byte alignment of `bytes` in array-of-struct. Per-pod CMS
// cost = WIDTH × DEPTH × DIR_MAX × 16 = 4 MiB.
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
//   STAT_EDT_DELAYED  ≤ STAT_THROTTLED  (egress with cfg.edt_pacing,
//                                         paced via skb->tstamp; first
//                                         choice when available)
//   STAT_ECN_MARKED   ≤ STAT_THROTTLED  (ECN-capable; ingress or egress
//                                         when EDT is off; marked CE,
//                                         passed)
//   STAT_DROPPED      ≤ STAT_THROTTLED  (non-ECN that neither EDT nor
//                                         ECN-mark could handle;
//                                         TC_ACT_SHOT)
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

#ifndef IP_MF
#define IP_MF     0x2000
#define IP_OFFSET 0x1FFF
#endif

// fold_in6 reduces an IPv6 address to 32 bits for the flow key. The
// key stays the same size for v4 and v6, so the per-row hash cost on
// the IPv4 path is unchanged. Collisions only merge flows in the CMS,
// which classifies them heavy sooner — the conservative direction.
static __always_inline __u32 fold_in6(const struct in6_addr *a)
{
	return a->in6_u.u6_addr32[0] ^ a->in6_u.u6_addr32[1] ^
	       a->in6_u.u6_addr32[2] ^ a->in6_u.u6_addr32[3];
}

// Build the flow key. Every packet gets one — nothing fails open:
//
//   - IPv4 / IPv6 with a TCP or UDP header in the linear area: full
//     5-tuple (v6 addresses folded, see fold_in6).
//   - IPv4 fragments: ports stay 0. Non-first fragments carry payload
//     where the ports would be, so parsing them would let a sender
//     mint a new "flow" per fragment. All fragments between a pair of
//     hosts share one flow.
//   - IPv6 with extension headers, other L4 protocols, or an L4 header
//     that's truncated: address pair + next header, ports 0.
//   - Non-IP or a truncated / malformed IP header: keyed on the
//     EtherType alone. ARP and the like stay mice; a raw-socket flood
//     of any EtherType becomes a heavy hitter and hits the bucket.
//
// Verifier-friendly: every pointer access is bounds-checked against
// `data_end`.
static __always_inline void parse_flow(struct __sk_buff *skb, struct flow_key *out)
{
	void *data     = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	void *l4;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return;

	if (eth->h_proto == bpf_htons(ETH_P_IP)) {
		struct iphdr *ip = (void *)(eth + 1);
		if ((void *)(ip + 1) > data_end || ip->ihl < 5)
			goto by_ethertype;
		out->src_ip = ip->saddr;
		out->dst_ip = ip->daddr;
		out->proto  = ip->protocol;
		if (ip->frag_off & bpf_htons(IP_MF | IP_OFFSET))
			return;
		l4 = (void *)ip + ((__u32)ip->ihl * 4);
	} else if (eth->h_proto == bpf_htons(ETH_P_IPV6)) {
		struct ipv6hdr *ip6 = (void *)(eth + 1);
		if ((void *)(ip6 + 1) > data_end)
			goto by_ethertype;
		out->src_ip = fold_in6(&ip6->saddr);
		out->dst_ip = fold_in6(&ip6->daddr);
		out->proto  = ip6->nexthdr;
		l4 = (void *)(ip6 + 1);
	} else {
		goto by_ethertype;
	}

	if (out->proto == IPPROTO_TCP) {
		struct tcphdr *th = l4;
		if ((void *)(th + 1) > data_end)
			return;
		out->src_port = th->source;
		out->dst_port = th->dest;
	} else if (out->proto == IPPROTO_UDP) {
		struct udphdr *uh = l4;
		if ((void *)(uh + 1) > data_end)
			return;
		out->src_port = uh->source;
		out->dst_port = uh->dest;
	}
	return;

by_ethertype:
	__builtin_memset(out, 0, sizeof(*out));
	out->src_port = eth->h_proto;
}

// mice_within_budget adds `len` to `dir`'s mouse-byte window and
// reports whether the window is still within budget. See
// natra_mice_map for the budget.
static __always_inline int mice_within_budget(__u32 dir, __u64 len, __u64 now_ns,
					      const struct natra_config *cfg)
{
	struct mice_window *mw = bpf_map_lookup_elem(&natra_mice_map, &dir);
	if (!mw)
		return 1;

	__u64 win = now_ns / MICE_WINDOW_NS;
	if (mw->idx != win) {
		mw->idx = win;
		mw->bytes = 0;
	}
	__sync_fetch_and_add(&mw->bytes, len);
	__u64 total = mw->bytes;

	__u64 floor = 0xffffffffffffffffULL;
	if (cfg->hh_threshold < (floor / MICE_BUDGET_HH))
		floor = cfg->hh_threshold * MICE_BUDGET_HH;
	if (total <= floor)
		return 1;
	// rate_bps is bytes/sec (the field name predates the unit), so
	// >> 3 is 125 ms of the configured rate — close to one window,
	// no divide, no overflow.
	return total <= (cfg->rate_bps >> 3);
}

// consume_tokens charges `bytes` against `dir`'s bucket. Returns 1 if
// the charge succeeded (caller passes the packet), 0 if the bucket
// lacks tokens (caller falls through to throttle_disposition — EDT
// on egress when cfg.edt_pacing, else ECN-mark on ECN-capable, else
// drop).
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
//   1. EDT pacing (egress with cfg->edt_pacing != 0). Compute a
//      release time per bytes/rate_bps, advance the bucket's
//      next_release_ns past it, stamp skb->tstamp; the downstream
//      fq qdisc holds the skb until that time. EDT preserves the
//      sender's TCP cwnd — no backoff, no retrans — and the
//      bucket releases at exactly rate_bps, so the measured
//      receiver bps converges to the configured rate.
//
//      EDT is preferred over ECN on egress because ECN-mark on
//      every above-rate packet repeatedly halves cwnd; the
//      sender backs off well below the cap, causing measurable
//      under-throttling. EDT alone keeps the flow at the cap.
//      RED-style probabilistic ECN would be a better blend, but
//      we don't currently have it.
//
//   2. ECN-mark via bpf_skb_ecn_set_ce. Helper returns 1 on an
//      ECN-capable packet (ECT(0) or ECT(1) in IP TOS bits 0-1)
//      and sets the CE bit. Reached on ingress (no fq downstream
//      so EDT doesn't apply) and on egress without EDT enabled.
//
//   3. Drop (TC_ACT_SHOT). Reached for non-ECN traffic that
//      neither EDT nor ECN-mark could handle.
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
	if (dir == DIR_EGRESS && edt_pacing != 0) {
		struct token_bucket *tb = bpf_map_lookup_elem(&natra_bucket_map, &dir);
		if (tb && rate_bps > 0) {
			// add_ns = bytes * 8 * 1e9 / rate_bps. Split so the
			// multiply stays inside u64 for MTU-sized packets at
			// modest rates (e.g., 1500 B at 1 Mbps → 12 ms).
			__u64 add_ns = (bytes * 8000ULL) * 1000000ULL / rate_bps;
			__u64 release_at;
			__u64 delay_ns;
			bpf_spin_lock(&tb->lock);
			__u64 base = tb->next_release_ns;
			if (base < now_ns)
				base = now_ns;
			release_at = base + add_ns;
			// Always advance the bucket's debt counter — the rate
			// cap stays correct regardless of how this packet is
			// eventually delivered (EDT / ECN / drop).
			tb->next_release_ns = release_at;
			bpf_spin_unlock(&tb->lock);
			delay_ns = release_at - now_ns;

			// Bounded EDT — see MAX_EDT_DELAY_NS comment up top.
			// Above the bound, fall through to ECN-mark or drop.
			if (delay_ns <= MAX_EDT_DELAY_NS) {
				skb->tstamp = release_at;
				bump_stat(dir, STAT_EDT_DELAYED);
				return TC_ACT_OK;
			}
			// Fall through to ECN-mark / drop below.
		}
	}

	if (bpf_skb_ecn_set_ce(skb) > 0) {
		bump_stat(dir, STAT_ECN_MARKED);
		return TC_ACT_OK;
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
	parse_flow(skb, &k);

	// One ktime read per packet — reused for CMS aging (now_idx) and
	// token bucket refill (now_ns). Avoids a second syscall.
	__u64 now_ns = bpf_ktime_get_ns();
	__u32 now_idx = (__u32)(now_ns / CMS_DECAY_INTERVAL_NS);

	__u64 len = skb->len;
	__u64 bytes_est = cms_update_and_min(dir, &k, now_idx, len);
	if (bytes_est <= cfg->hh_threshold &&
	    mice_within_budget(dir, len, now_ns, cfg)) {
		// Mouse: fast pass with no lock. Low-volume traffic stays
		// at line rate even when an elephant exists on the same pod.
		// Past the pod's mouse budget, mice fall through to the
		// bucket below.
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}

	// Heavy hitter — token bucket gate.
	bump_stat(dir, STAT_HH_HITS);
	if (consume_tokens(dir, len, cfg->rate_bps, cfg->burst_bytes, now_ns)) {
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}
	// Above the rate: on egress with cfg->edt_pacing set, EDT-pace
	// the packet (preserves cwnd; fq downstream honors skb->tstamp).
	// Otherwise ECN-mark on ECN-capable, drop only as last resort.
	// STAT_THROTTLED is the cardinality of all overflow events; the
	// disposition stat (ECN_MARKED / EDT_DELAYED / DROPPED) records
	// the outcome.
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
