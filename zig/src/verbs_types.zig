//! What the verbs implementations return to capi.zig: verbs_linux.zig on
//! Linux, verbs_stub.zig elsewhere. One definition for both, so they cannot
//! drift apart; verbs.zig checks each implementation's function signatures
//! against the same list at compile time.
const portaddr = @import("portaddr.zig");

pub const LoopResult = struct {
    small_p50: u64,
    small_p99: u64,
    large_p50: u64,
    bw: u64,
    completions: u32,
    /// ToN'24 §VI-C verb execution times of this probe's setup, in ns.
    /// gpu_alloc_ns is 0 for a memory endpoint.
    gpu_alloc_ns: u64 = 0,
    mr_reg_ns: u64 = 0,
    qp_setup_ns: u64 = 0,
    /// Time the --mem-warmup fill ran before the timed iterations; 0 when
    /// off or for a GPU endpoint.
    mem_warmup_ns: u64 = 0,
    /// How the probe addressed its port, the port it used and, for an
    /// InfiniBand port, its LID (0 on RoCE).
    mode: portaddr.Mode = .none,
    port: u8 = 0,
    lid: u16 = 0,
};

pub const MutualResult = struct {
    rtt_p50: u64,
    rtt_p99: u64,
    sent: u32,
    recvd: u32,
    connected: i32,
    reliable: i32,
};

/// Size of each registered mutual-probe buffer; the largest SEND payload.
/// hostping_mutual and hostping_ud_open reject a longer msg_size up front.
pub const mutual_buf_bytes: u32 = 4096;
