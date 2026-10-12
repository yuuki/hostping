const std = @import("std");

pub const LoopResult = struct {
    small_p50: u64,
    small_p99: u64,
    large_p50: u64,
    bw: u64,
    completions: u32,
    gpu_alloc_ns: u64 = 0,
    mr_reg_ns: u64 = 0,
    qp_setup_ns: u64 = 0,
    /// Time the --mem-warmup fill ran before the timed iterations; 0 when
    /// off or for a GPU endpoint.
    mem_warmup_ns: u64 = 0,
    mode: @import("portaddr.zig").Mode = .none,
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

var stub_detail: [32]u8 = @splat(0);

pub fn lastQpDetail() []const u8 {
    return std.mem.sliceTo(&stub_detail, 0);
}

pub fn lastPortFault() bool {
    return false;
}

pub fn loopbackVerbs(_: []const u8, _: i32, gpu_index: i32, gpu_bdf: []const u8, _: u32, _: u32, _: u32, _: u32, _: u64) !LoopResult {
    @memset(&stub_detail, 0);
    if (gpu_index >= 0) {
        const msg: []const u8 = if (gpu_bdf.len == 0) "NoGDR: missing gpu" else "NoGDR: no cuda";
        const n = @min(msg.len, stub_detail.len - 1);
        @memcpy(stub_detail[0..n], msg[0..n]);
        return error.NoGDR;
    }
    return error.NoDevice;
}

pub fn mutualVerbsRc(_: []const u8, _: []const u8, _: u32, _: u32, _: u32) !MutualResult {
    return error.NoDevice;
}

pub fn mutualVerbsUd(_: []const u8, _: []const u8, _: u32, _: u32, _: u32) !MutualResult {
    return error.NoDevice;
}

const udtag = @import("udtag.zig");

/// The UD mutual probe needs libibverbs; off Linux there is no endpoint.
pub const UdEp = opaque {};

pub fn udOpen(_: []const u8, _: u32) !*UdEp {
    return error.NoDevice;
}

pub fn udClose(_: *UdEp) void {}

pub fn udSend(_: *UdEp, _: *const UdEp, _: u32) !u64 {
    return error.NoDevice;
}

pub fn udWait(_: []const ?*UdEp, _: u64, _: u32, _: []udtag.Arrival) !usize {
    return error.NoDevice;
}
