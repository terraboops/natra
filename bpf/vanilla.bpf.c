// SPDX-License-Identifier: GPL-2.0
//
// Vanilla bandwidth emulator — used only by the L5 head-to-head test.
//
// This program emulates what containernetworking/plugins/bandwidth
// does: token-bucket rate-limiting applied to every packet, no flow
// awareness. The upstream plugin uses kernel HTB on an IFB device;
// from the perspective of "what fraction of mice survive when an
// elephant is present" that's the same shape as a global token
// bucket — the elephant exhausts tokens, mice starve.
//
// Two SEC programs (vanilla_ingress, vanilla_egress) and per-direction
// state, mirroring natra.bpf.c, so the L5 head-to-head can compare
// both directions symmetrically. Production never loads this.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

enum vanilla_direction {
	VAN_DIR_INGRESS = 0,
	VAN_DIR_EGRESS  = 1,
	VAN_DIR_MAX     = 2,
};

struct vanilla_config {
	__u64 rate_bps;
	__u64 burst_bytes;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct vanilla_config);
	__uint(max_entries, VAN_DIR_MAX);
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
	__uint(max_entries, VAN_DIR_MAX);
} vanilla_bucket_map SEC(".maps");

enum {
	VAN_STAT_PASSED    = 0,
	VAN_STAT_THROTTLED = 1,
	VAN_STAT_PER_DIR,
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, VAN_STAT_PER_DIR * VAN_DIR_MAX);
} vanilla_stats_map SEC(".maps");

static __always_inline void van_bump(__u32 dir, __u32 slot)
{
	__u32 idx = dir * VAN_STAT_PER_DIR + slot;
	__u64 *v = bpf_map_lookup_elem(&vanilla_stats_map, &idx);
	if (v)
		(*v)++;
}

static __always_inline int vanilla_classify(struct __sk_buff *skb, __u32 dir)
{
	struct vanilla_config *cfg = bpf_map_lookup_elem(&vanilla_config_map, &dir);
	if (!cfg || cfg->rate_bps == 0) {
		van_bump(dir, VAN_STAT_PASSED);
		return TC_ACT_OK;
	}
	struct vanilla_bucket *tb = bpf_map_lookup_elem(&vanilla_bucket_map, &dir);
	if (!tb) {
		van_bump(dir, VAN_STAT_PASSED);
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
		van_bump(dir, VAN_STAT_PASSED);
		return TC_ACT_OK;
	}
	van_bump(dir, VAN_STAT_THROTTLED);
	return TC_ACT_SHOT;
}

SEC("tc")
int vanilla_ingress(struct __sk_buff *skb)
{
	return vanilla_classify(skb, VAN_DIR_INGRESS);
}

SEC("tc")
int vanilla_egress(struct __sk_buff *skb)
{
	return vanilla_classify(skb, VAN_DIR_EGRESS);
}

char __license[] SEC("license") = "GPL";
