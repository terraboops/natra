// SPDX-License-Identifier: GPL-2.0
//
// Vanilla bandwidth emulator — for the L5 head-to-head comparison.
//
// Mimics what the upstream containernetworking/plugins/bandwidth
// plugin does: token-bucket rate-limiting applied UNIFORMLY to every
// packet, no flow-cardinality awareness. The upstream plugin uses
// kernel HTB (Hierarchical Token Bucket) qdisc on an IFB device; HTB
// classifies all traffic into one class and enforces the rate. From
// the perspective of "what fraction of mice flows survive when an
// elephant is also present", that's algorithmically identical to a
// global token bucket — the elephant exhausts tokens, mice starve.
//
// This program is the "control" in the natra-vs-vanilla comparison.
// Same input traffic, same rate config, no CMS bypass — the only
// difference is the mouse-flow fast-pass that natra has and vanilla
// doesn't. The L5 TestScenarioMixedVsVanilla driver runs identical
// packet sequences through both this program and the real
// natra.bpf.o, then asserts mice goodput differs in natra's favor.
//
// IMPORTANT: This is NOT what natra ships. It exists exclusively for
// proof-by-comparison in tests. Production never loads it.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

struct vanilla_config {
	__u64 rate_bps;
	__u64 burst_bytes;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct vanilla_config);
	__uint(max_entries, 1);
} vanilla_config_map SEC(".maps");

struct vanilla_bucket {
	struct bpf_spin_lock lock;
	__u64 tokens;
	__u64 last_update_ns;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct vanilla_bucket);
	__uint(max_entries, 1);
} vanilla_bucket_map SEC(".maps");

enum {
	VAN_STAT_PASSED    = 0,
	VAN_STAT_THROTTLED = 1,
	VAN_STAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, VAN_STAT_MAX);
} vanilla_stats_map SEC(".maps");

static __always_inline void van_bump(__u32 idx)
{
	__u64 *v = bpf_map_lookup_elem(&vanilla_stats_map, &idx);
	if (v)
		(*v)++;
}

SEC("tc")
int vanilla_ratelimit(struct __sk_buff *skb)
{
	__u32 zero = 0;
	struct vanilla_config *cfg = bpf_map_lookup_elem(&vanilla_config_map, &zero);
	if (!cfg || cfg->rate_bps == 0) {
		van_bump(VAN_STAT_PASSED);
		return TC_ACT_OK;
	}
	struct vanilla_bucket *tb = bpf_map_lookup_elem(&vanilla_bucket_map, &zero);
	if (!tb) {
		van_bump(VAN_STAT_PASSED);
		return TC_ACT_OK;
	}

	__u64 now = bpf_ktime_get_ns();
	int allowed = 0;
	bpf_spin_lock(&tb->lock);
	__u64 elapsed_ns = now - tb->last_update_ns;
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
		van_bump(VAN_STAT_PASSED);
		return TC_ACT_OK;
	}
	van_bump(VAN_STAT_THROTTLED);
	return TC_ACT_SHOT;
}

char __license[] SEC("license") = "GPL";
