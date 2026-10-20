//! The verbs implementation off Linux, where there is no libibverbs: every
//! probe fails with NoDevice, a GPU endpoint with NoGDR. verbs.zig checks
//! its signatures against the same list as verbs_linux.zig's at compile
//! time, and root_test.zig analyses its bodies, so a Linux build catches a
//! stub that drifted.
const detail = @import("detail.zig");
const udtag = @import("udtag.zig");
const types = @import("verbs_types.zig");

const LoopResult = types.LoopResult;
const MutualResult = types.MutualResult;

var stub_detail: detail.Detail = .{};

pub fn lastQpDetail() []const u8 {
    return stub_detail.get();
}

pub fn lastPortFault() bool {
    return false;
}

pub fn loopbackVerbs(_: []const u8, _: i32, gpu_index: i32, gpu_bdf: []const u8, _: u32, _: u32, _: u32, _: u32, _: u64) !LoopResult {
    stub_detail.clear();
    if (gpu_index >= 0) {
        const msg: []const u8 = if (gpu_bdf.len == 0) "missing gpu" else "no cuda";
        stub_detail.set("{s}", .{msg});
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
