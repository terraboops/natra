// SPDX-License-Identifier: GPL-2.0
//
// Intentionally-invalid BPF program: reads packet data without
// bounds-checking against skb->data_end. The verifier must reject
// this with "invalid access to packet" or similar — the test loads
// this object and asserts a *ebpf.VerifierError is returned.
//
// This is the canonical "did natra report verifier errors clearly?"
// fixture for the L3 chaos suite.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

SEC("tc")
int invalid_oob_packet_access(struct __sk_buff *skb)
{
	void *data = (void *)(long)skb->data;
	// No bounds check vs skb->data_end before this read. The verifier
	// must reject — packet data accesses require an explicit comparison
	// of `ptr + offset <= data_end` first.
	char *p = data;
	return p[1000];
}

char __license[] SEC("license") = "GPL";
