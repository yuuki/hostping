//! The C ABI shared with cgo. The `extern struct`s below are the canonical
//! Zig view; `include/hostping.h` is the canonical C view. They are written by
//! hand in both places, so this module compares them against each other at
//! compile time: size, alignment and every field offset, plus the
//! HOSTPING_* constants. Adding a field to one side without the other, or
//! reordering fields, is a build error rather than silent memory corruption.
const std = @import("std");
const errclass = @import("errclass.zig");
const udtag = @import("udtag.zig");
const portaddr = @import("portaddr.zig");

const c = @import("hostping_h");

pub const NAME_LEN = 64;
pub const ERR_LEN = 256;

pub const LoopbackReq = extern struct {
    device: [NAME_LEN]u8,
    endpoint: [NAME_LEN]u8,
    numa_node: i32,
    gpu_index: i32,
    small_size: u32,
    large_size: u32,
    iters: u32,
    timeout_ms: u32,
    sim: i32,
    sim_path_bw_bps: u64,
    sim_path_lat_ns: u64,
    sim_tproc_ns: u64,
    sim_loss_ppm: u64,
    gpu_bdf: [NAME_LEN]u8,
    sim_gpu_alloc_ns: u64,
    sim_mr_reg_ns: u64,
    sim_qp_setup_ns: u64,
    mem_warmup_ns: u64,
};

pub const LoopbackRes = extern struct {
    rc: i32,
    err: [ERR_LEN]u8,
    small_lat_ns_p50: u64,
    small_lat_ns_p99: u64,
    large_lat_ns_p50: u64,
    bandwidth_bps: u64,
    completions: u32,
    gpu_alloc_ns: u64,
    mr_reg_ns: u64,
    qp_setup_ns: u64,
    mem_warmup_ns: u64,
    addr_mode: i32,
    port_num: u32,
    lid: u32,
    port_fault: u32,
};

pub const MutualReq = extern struct {
    src_device: [NAME_LEN]u8,
    dst_device: [NAME_LEN]u8,
    msg_size: u32,
    iters: u32,
    timeout_ms: u32,
    sim: i32,
    sim_rtt_ns: u64,
    sim_loss_ppm: u32,
    sim_connect_fail: i32,
    transport: i32,
};

pub const MutualRes = extern struct {
    rc: i32,
    err: [ERR_LEN]u8,
    rtt_ns_p50: u64,
    rtt_ns_p99: u64,
    sent: u32,
    recvd: u32,
    drops: u32,
    loss_ratio: f64,
    connected: i32,
    reliable: i32,
};

pub const UdReply = extern struct {
    ep: u32,
    seq: u32,
    t_ns: u64,
};

pub const UD_MAX_EPS = 128;
pub const MUTUAL_RC = 0;
pub const MUTUAL_UD = 1;

/// Fails the build unless `Zig` and `C` have identical layout: same size,
/// same alignment, same field set and the same offset and width for every
/// field. Iterating the @typeInfo field names means a newly added field is
/// checked without touching this function.
fn verifyLayout(comptime Zig: type, comptime C: type, comptime name: []const u8) void {
    const zig_fields = @typeInfo(Zig).@"struct".field_names;
    const c_fields = @typeInfo(C).@"struct".field_names;
    if (zig_fields.len != c_fields.len) {
        @compileError(std.fmt.comptimePrint(
            "ABI mismatch in {s}: Zig has {d} fields, hostping.h has {d}",
            .{ name, zig_fields.len, c_fields.len },
        ));
    }
    if (@sizeOf(Zig) != @sizeOf(C)) {
        @compileError(std.fmt.comptimePrint(
            "ABI mismatch in {s}: @sizeOf Zig {d} != C {d}",
            .{ name, @sizeOf(Zig), @sizeOf(C) },
        ));
    }
    if (@alignOf(Zig) != @alignOf(C)) {
        @compileError(std.fmt.comptimePrint(
            "ABI mismatch in {s}: @alignOf Zig {d} != C {d}",
            .{ name, @alignOf(Zig), @alignOf(C) },
        ));
    }
    inline for (zig_fields) |field_name| {
        if (!@hasField(C, field_name)) {
            @compileError(std.fmt.comptimePrint(
                "ABI mismatch in {s}: field '{s}' is missing from hostping.h",
                .{ name, field_name },
            ));
        }
        const ZigField = @FieldType(Zig, field_name);
        const CField = @FieldType(C, field_name);
        if (@offsetOf(Zig, field_name) != @offsetOf(C, field_name)) {
            @compileError(std.fmt.comptimePrint(
                "ABI mismatch in {s}.{s}: @offsetOf Zig {d} != C {d}",
                .{ name, field_name, @offsetOf(Zig, field_name), @offsetOf(C, field_name) },
            ));
        }
        if (@sizeOf(ZigField) != @sizeOf(CField)) {
            @compileError(std.fmt.comptimePrint(
                "ABI mismatch in {s}.{s}: @sizeOf Zig {d} != C {d}",
                .{ name, field_name, @sizeOf(ZigField), @sizeOf(CField) },
            ));
        }
        if (@alignOf(ZigField) != @alignOf(CField)) {
            @compileError(std.fmt.comptimePrint(
                "ABI mismatch in {s}.{s}: @alignOf Zig {d} != C {d}",
                .{ name, field_name, @alignOf(ZigField), @alignOf(CField) },
            ));
        }
    }
}

fn verifyConst(comptime name: []const u8, zig_value: comptime_int, c_value: comptime_int) void {
    if (zig_value != c_value) {
        @compileError(std.fmt.comptimePrint(
            "ABI mismatch in {s}: Zig {d} != hostping.h {d}",
            .{ name, zig_value, c_value },
        ));
    }
}

comptime {
    verifyLayout(LoopbackReq, c.hostping_loopback_req, "hostping_loopback_req");
    verifyLayout(LoopbackRes, c.hostping_loopback_res, "hostping_loopback_res");
    verifyLayout(MutualReq, c.hostping_mutual_req, "hostping_mutual_req");
    verifyLayout(MutualRes, c.hostping_mutual_res, "hostping_mutual_res");
    verifyLayout(UdReply, c.hostping_ud_reply, "hostping_ud_reply");
    verifyConst("HOSTPING_UD_MAX_EPS", UD_MAX_EPS, c.HOSTPING_UD_MAX_EPS);
    verifyConst("HOSTPING_MUTUAL_RC", MUTUAL_RC, c.HOSTPING_MUTUAL_RC);
    verifyConst("HOSTPING_MUTUAL_UD", MUTUAL_UD, c.HOSTPING_MUTUAL_UD);
    verifyConst("udtag.max_endpoints", UD_MAX_EPS, udtag.max_endpoints);
    verifyConst("HOSTPING_ADDR_NONE", @backingInt(portaddr.Mode.none), c.HOSTPING_ADDR_NONE);
    verifyConst("HOSTPING_ADDR_ROCEV2_GID", @backingInt(portaddr.Mode.rocev2_gid), c.HOSTPING_ADDR_ROCEV2_GID);
    verifyConst("HOSTPING_ADDR_IB_LID", @backingInt(portaddr.Mode.ib_lid), c.HOSTPING_ADDR_IB_LID);
    verifyConst("HOSTPING_ADDR_IB_LID_GRH", @backingInt(portaddr.Mode.ib_lid_grh), c.HOSTPING_ADDR_IB_LID_GRH);

    verifyConst("HOSTPING_NAME_LEN", NAME_LEN, c.HOSTPING_NAME_LEN);
    verifyConst("HOSTPING_ERR_LEN", ERR_LEN, c.HOSTPING_ERR_LEN);

    // The rc values capi.zig returns come from errclass.verbsRC and from the
    // literals in hostping_mutual; both must be the header's HOSTPING_ERR_*.
    verifyConst("HOSTPING_OK", 0, c.HOSTPING_OK);
    verifyConst("HOSTPING_ERR", 1, c.HOSTPING_ERR);
    verifyConst("HOSTPING_ERR_NO_DEVICE", errclass.verbsRC(error.NoDevice), c.HOSTPING_ERR_NO_DEVICE);
    verifyConst("HOSTPING_ERR_VERBS", errclass.verbsRC(error.Pd), c.HOSTPING_ERR_VERBS);
    verifyConst("HOSTPING_ERR_TIMEOUT", errclass.verbsRC(error.Timeout), c.HOSTPING_ERR_TIMEOUT);
    verifyConst("HOSTPING_ERR_CONNECT", errclass.verbsRC(error.Connect), c.HOSTPING_ERR_CONNECT);
    verifyConst("HOSTPING_ERR_NO_GDR", errclass.verbsRC(error.NoGDR), c.HOSTPING_ERR_NO_GDR);
    verifyConst("HOSTPING_ERR_PORT", errclass.verbsRC(error.Gid), c.HOSTPING_ERR_PORT);
}

test "C header and Zig structs agree on layout" {
    // The comptime block above is the real check; referencing the types here
    // makes `zig build test` force its analysis.
    try std.testing.expectEqual(@sizeOf(c.hostping_loopback_req), @sizeOf(LoopbackReq));
    try std.testing.expectEqual(@sizeOf(c.hostping_loopback_res), @sizeOf(LoopbackRes));
    try std.testing.expectEqual(@sizeOf(c.hostping_mutual_req), @sizeOf(MutualReq));
    try std.testing.expectEqual(@sizeOf(c.hostping_mutual_res), @sizeOf(MutualRes));
    try std.testing.expectEqual(@sizeOf(c.hostping_ud_reply), @sizeOf(UdReply));
}
