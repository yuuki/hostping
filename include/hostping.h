#ifndef HOSTPING_H
#define HOSTPING_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

#define HOSTPING_ERR_LEN 256
#define HOSTPING_NAME_LEN 64
#define HOSTPING_OK 0
#define HOSTPING_ERR 1
#define HOSTPING_ERR_NO_DEVICE 2
#define HOSTPING_ERR_VERBS 3
/* HOSTPING_ERR_TIMEOUT and HOSTPING_ERR_CONNECT are the connectivity classes:
 * the path itself failed (QP handshake, completion error or timeout). Only
 * these make hostping_mutual report a measured "not connected" result; every
 * other code is a local setup failure that says nothing about the path.
 * err starts with the Zig error name ("Connect: ..."); the Go side
 * (internal/zigprobe) sub-classifies HOSTPING_ERR_CONNECT and
 * HOSTPING_ERR_PORT by that leading name, so the names are part of the ABI. */
#define HOSTPING_ERR_TIMEOUT 4
#define HOSTPING_ERR_CONNECT 5
#define HOSTPING_ERR_NO_GDR 6
/* No usable port address: no active RoCEv2 GID on a RoCE port, or an
 * InfiniBand port that is not ACTIVE or has no LID. err names which, by its
 * leading Zig error name (NoLid, PortDown, Port), which Go reads. */
#define HOSTPING_ERR_PORT 7

/* hostping_loopback_res.addr_mode: how a live probe addressed its port. */
#define HOSTPING_ADDR_NONE 0       /* no live probe (sim) */
#define HOSTPING_ADDR_ROCEV2_GID 1 /* RoCEv2 GID, GRH */
#define HOSTPING_ADDR_IB_LID 2     /* InfiniBand LID, no GRH */
#define HOSTPING_ADDR_IB_LID_GRH 3 /* InfiniBand LID plus a GRH over GID 0 */

typedef struct hostping_loopback_req {
    char device[HOSTPING_NAME_LEN];
    char endpoint[HOSTPING_NAME_LEN];
    int32_t numa_node;
    int32_t gpu_index;
    uint32_t small_size;
    uint32_t large_size;
    uint32_t iters;
    uint32_t timeout_ms;
    int32_t sim;
    uint64_t sim_path_bw_bps;
    uint64_t sim_path_lat_ns;
    uint64_t sim_tproc_ns;
    uint64_t sim_loss_ppm;
    char gpu_bdf[HOSTPING_NAME_LEN];
    /* Verb execution times the simulator reports back verbatim (sim != 0). */
    uint64_t sim_gpu_alloc_ns;
    uint64_t sim_mr_reg_ns;
    uint64_t sim_qp_setup_ns;
    /* --mem-warmup: for a memory endpoint (gpu_index < 0), write both
     * NUMA-bound buffers from the CPU with non-temporal stores for this long
     * right before the timed iterations; 0 = off, capped at 1 s. Ignored for
     * GPU endpoints and by the simulator. */
    uint64_t mem_warmup_ns;
} hostping_loopback_req;

typedef struct hostping_loopback_res {
    int32_t rc;
    char err[HOSTPING_ERR_LEN];
    uint64_t small_lat_ns_p50;
    uint64_t small_lat_ns_p99;
    uint64_t large_lat_ns_p50;
    uint64_t bandwidth_bps;
    uint32_t completions;
    /* ToN'24 VI-C verb execution times of the probe's setup, in ns:
     * GPU allocation (0 for a memory endpoint), MR registration summed over
     * every MR, and QP create + INIT/RTR/RTS including GID retries. */
    uint64_t gpu_alloc_ns;
    uint64_t mr_reg_ns;
    uint64_t qp_setup_ns;
    /* Time the memory warm-up actually ran (0 when off or for a GPU endpoint). */
    uint64_t mem_warmup_ns;
    /* HOSTPING_ADDR_*, the port the QPs were bound to, and the port's LID
     * on InfiniBand (0 on RoCE). */
    int32_t addr_mode;
    uint32_t port_num;
    uint32_t lid;
    /* Nonzero when a failed probe failed at an InfiniBand port: its loopback
     * WRITEs completed with a port-level transport error (retry counter
     * exceeded, a remote error, a response timeout) in every addressing mode
     * it tried. Other probes of the same port would fail the same way. */
    uint32_t port_fault;
} hostping_loopback_res;

typedef struct hostping_mutual_req {
    char src_device[HOSTPING_NAME_LEN];
    char dst_device[HOSTPING_NAME_LEN];
    uint32_t msg_size;
    uint32_t iters;
    uint32_t timeout_ms;
    int32_t sim;
    uint64_t sim_rtt_ns;
    uint32_t sim_loss_ppm;
    int32_t sim_connect_fail;
    /* HOSTPING_MUTUAL_RC (0): RC SEND ping-pong; RC retransmission hides an
     * isolated loss. HOSTPING_MUTUAL_UD (1): UD request/reply, where one lost
     * packet is one timed-out probe (ToN'24 IV-A-2). */
    int32_t transport;
} hostping_mutual_req;

#define HOSTPING_MUTUAL_RC 0
#define HOSTPING_MUTUAL_UD 1

typedef struct hostping_mutual_res {
    int32_t rc;
    char err[HOSTPING_ERR_LEN];
    uint64_t rtt_ns_p50;
    uint64_t rtt_ns_p99;
    uint32_t sent;
    uint32_t recvd;
    uint32_t drops;
    double loss_ratio;
    int32_t connected;
    /* 1 = live verbs probe (RC or UD): a lost probe is a timeout
     * (recvd < sent), and loss_ratio is not Ethernet drop evidence. */
    int32_t reliable;
} hostping_mutual_res;

/* The scheduled mutual probe (ToN'24 IV-A-2): one UD endpoint per RNIC, kept
 * open for the life of the probe loop. A probe is a UD request from endpoint
 * src to endpoint dst, answered from dst by hostping_ud_wait; the reply lands
 * back on src. Endpoints are addressed by their index in the array every call
 * passes, so the array must stay in the same order (NULL for an endpoint that
 * failed to open) and hold at most HOSTPING_UD_MAX_EPS entries. None of these
 * calls is thread-safe with another call on the same endpoints. */
#define HOSTPING_UD_MAX_EPS 128

typedef struct hostping_ud_ep hostping_ud_ep;

typedef struct hostping_ud_reply {
    uint32_t ep;   /* index of the requesting endpoint */
    uint32_t seq;  /* the probe's sequence number, mod 2^24 */
    uint64_t t_ns; /* CLOCK_MONOTONIC time the reply was polled */
} hostping_ud_reply;

int hostping_ud_open(const char *device, uint32_t msg_size, hostping_ud_ep **out, char *err);
void hostping_ud_close(hostping_ud_ep *ep);
/* Posts probe seq from eps[src] to eps[dst]; *t_ns is when it was posted. */
int hostping_ud_send(hostping_ud_ep *const *eps, uint32_t n, uint32_t src, uint32_t dst, uint32_t seq, uint64_t *t_ns, char *err);
/* Answers requests and collects replies: spins up to spin_ns, then sleeps up
 * to block_ms, returning as soon as a reply is in. *nout is the reply count. */
int hostping_ud_wait(hostping_ud_ep *const *eps, uint32_t n, uint64_t spin_ns, uint32_t block_ms, hostping_ud_reply *out, uint32_t max, uint32_t *nout, char *err);

const char *hostping_version(void);
int hostping_loopback(const hostping_loopback_req *req, hostping_loopback_res *res);
/* Releases the GPU runtime contexts that loopback probes created and no
 * longer use. Call between runs so an idle process holds no GPU context. */
void hostping_gpu_release(void);
int hostping_mutual(const hostping_mutual_req *req, hostping_mutual_res *res);
uint64_t hostping_sim_latency_ns(uint64_t tproc_ns, uint64_t lathost_ns, uint64_t size_bytes, uint64_t bw_bps);
uint64_t hostping_sim_bandwidth_bps(uint64_t size_bytes, uint64_t lat_large_ns, uint64_t lat_small_ns);

#ifdef __cplusplus
}
#endif

#endif
