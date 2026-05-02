// SPDX-License-Identifier: Apache-2.0
//
// natra dataplane — Phase 1 step 1: minimal token-bucket rate limiter.
//
// This is the smallest BPF program that does something useful. It
// charges every skb's length against a single global token bucket,
// drops if empty, passes if not. No CMS, no flow parsing yet — those
// land in step 2 once this loads, attaches, and rate-limits cleanly.
//
// Userspace populates `natra_config_map` (one entry: rate_bps,
// burst_bytes) before traffic arrives. Token bucket state lives in
// `natra_bucket_map`, protected by a bpf_spin_lock so concurrent CPUs
// can't double-spend.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

struct natra_config {
	__u64 rate_bps;
	__u64 burst_bytes;
	__u64 hh_threshold; // unused in step 1; reserved for CMS in step 2
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct natra_config);
	__uint(max_entries, 1);
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
	__uint(max_entries, 1);
} natra_bucket_map SEC(".maps");

enum {
	STAT_PASSED    = 0,
	STAT_THROTTLED = 1,
	STAT_HH_HITS   = 2,
	STAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, STAT_MAX);
} natra_stats_map SEC(".maps");

static __always_inline void bump_stat(__u32 idx)
{
	__u64 *v = bpf_map_lookup_elem(&natra_stats_map, &idx);
	if (v)
		(*v)++;
}

SEC("tc")
int natra_ratelimit(struct __sk_buff *skb)
{
	__u32 zero = 0;
	struct natra_config *cfg = bpf_map_lookup_elem(&natra_config_map, &zero);
	if (!cfg || cfg->rate_bps == 0) {
		bump_stat(STAT_PASSED);
		return TC_ACT_OK;
	}

	struct token_bucket *tb = bpf_map_lookup_elem(&natra_bucket_map, &zero);
	if (!tb) {
		bump_stat(STAT_PASSED);
		return TC_ACT_OK;
	}

	// Call helpers (bpf_ktime_get_ns) BEFORE the spin lock — the verifier
	// forbids most helper calls inside a locked critical section.
	__u64 now = bpf_ktime_get_ns();
	int allowed = 0;
	bpf_spin_lock(&tb->lock);
	__u64 elapsed_ns = now - tb->last_update_ns;
	// rate_bps * elapsed_ns / 1e9, computed in two steps to avoid overflow
	// for long idle periods. The cap-by-burst happens after; if we idle for
	// hours, we don't accumulate hours of tokens — we cap at `burst_bytes`.
	__u64 added = (elapsed_ns / 1000ULL) * cfg->rate_bps / 1000000ULL;
	__u64 tokens = tb->tokens + added;
	if (tokens > cfg->burst_bytes)
		tokens = cfg->burst_bytes;
	if (tokens >= skb->len) {
		tokens -= skb->len;
		allowed = 1;
	}
	tb->tokens = tokens;
	tb->last_update_ns = now;
	bpf_spin_unlock(&tb->lock);

	if (allowed) {
		bump_stat(STAT_PASSED);
		return TC_ACT_OK;
	}
	bump_stat(STAT_THROTTLED);
	return TC_ACT_SHOT;
}

char __license[] SEC("license") = "GPL";
