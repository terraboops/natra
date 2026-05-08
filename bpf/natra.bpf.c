// SPDX-License-Identifier: GPL-2.0
//
// natra dataplane — CMS-driven heavy-hitter detection plus a token
// bucket on heavy traffic. Two directions.
//
// Stage 1 (Count-Min Sketch): every packet's 5-tuple flow key is
// hashed CMS_DEPTH times, each into one column of CMS_WIDTH. The min
// across rows is the per-flow count estimator. Constant memory
// (CMS_WIDTH * CMS_DEPTH * DIR_MAX = 8192 u32 counters) regardless of
// how many distinct flows the pod sees.
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
// fixed at load time. 1024 × 4 = 4096 u32 counters per direction =
// 16 KiB; covers any practical pod's flow cardinality with FP rate
// ~e/width = ~0.27%.
#define CMS_WIDTH 1024
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

struct natra_config {
	__u64 rate_bps;
	__u64 burst_bytes;
	__u64 hh_threshold; // CMS count above which a flow is "heavy"
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct natra_config);
	__uint(max_entries, DIR_MAX);
} natra_config_map SEC(".maps");

struct token_bucket {
	struct bpf_spin_lock lock;
	__u64 tokens;
	__u64 last_update_ns;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct token_bucket);
	__uint(max_entries, DIR_MAX);
} natra_bucket_map SEC(".maps");

// CMS as a flat array; index = dir * CMS_WIDTH * CMS_DEPTH +
// row * CMS_WIDTH + col. Per-direction halves are independent.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u32);
	__uint(max_entries, CMS_WIDTH * CMS_DEPTH * DIR_MAX);
} natra_cms_map SEC(".maps");

// Stat slots per direction. Total slots = STAT_PER_DIR * DIR_MAX.
// Userspace key = dir * STAT_PER_DIR + slot.
enum {
	STAT_PASSED    = 0,
	STAT_THROTTLED = 1,
	STAT_HH_HITS   = 2,
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

// Increment all CMS_DEPTH counters in `dir`'s half of the array and
// return the post-increment min across rows (CMS estimator).
static __always_inline __u32 cms_update_and_min(__u32 dir, const struct flow_key *k)
{
	__u32 base = dir * (CMS_WIDTH * CMS_DEPTH);
	__u32 mn = 0xffffffffu;
	#pragma unroll
	for (int row = 0; row < CMS_DEPTH; row++) {
		__u32 h = cms_hash(k, cms_seeds[row]);
		__u32 col = h % CMS_WIDTH;
		__u32 idx = base + (__u32)row * CMS_WIDTH + col;
		__u32 *cell = bpf_map_lookup_elem(&natra_cms_map, &idx);
		if (!cell)
			return 0;
		__u32 v = __sync_add_and_fetch(cell, 1);
		if (v < mn)
			mn = v;
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
// lacks tokens (caller drops). Refill is computed lazily from elapsed
// time so the lock is held for ~tens of ns per packet.
static __always_inline int consume_tokens(__u32 dir, __u64 bytes, __u64 rate_bps, __u64 burst, __u64 now)
{
	struct token_bucket *tb = bpf_map_lookup_elem(&natra_bucket_map, &dir);
	if (!tb)
		return 1;

	int allowed = 0;
	bpf_spin_lock(&tb->lock);
	__u64 elapsed_ns = now - tb->last_update_ns;
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
	tb->last_update_ns = now;
	bpf_spin_unlock(&tb->lock);
	return allowed;
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

	__u32 count = cms_update_and_min(dir, &k);
	if (count <= cfg->hh_threshold) {
		// Mouse: fast pass with no lock. Low-volume traffic stays
		// at line rate even when an elephant exists on the same pod.
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}

	// Heavy hitter — token bucket gate.
	bump_stat(dir, STAT_HH_HITS);
	__u64 now = bpf_ktime_get_ns();
	if (consume_tokens(dir, skb->len, cfg->rate_bps, cfg->burst_bytes, now)) {
		bump_stat(dir, STAT_PASSED);
		return TC_ACT_OK;
	}
	bump_stat(dir, STAT_THROTTLED);
	return TC_ACT_SHOT;
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
