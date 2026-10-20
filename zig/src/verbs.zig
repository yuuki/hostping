//! The verbs implementation behind capi.zig: libibverbs on Linux
//! (verbs_linux.zig), a stub that fails every probe elsewhere
//! (verbs_stub.zig). Both export the functions re-exported below; the
//! comptime block at the end fails the build when either one's signatures
//! drift from the list in verifyImpl, the stub included on a Linux build.
const builtin = @import("builtin");
const std = @import("std");
const udtag = @import("udtag.zig");
const types = @import("verbs_types.zig");

const impl = if (builtin.os.tag == .linux)
    @import("verbs_linux.zig")
else
    @import("verbs_stub.zig");

pub const LoopResult = types.LoopResult;
pub const MutualResult = types.MutualResult;
pub const loopbackVerbs = impl.loopbackVerbs;
pub const mutualVerbsRc = impl.mutualVerbsRc;
pub const mutualVerbsUd = impl.mutualVerbsUd;
pub const lastQpDetail = impl.lastQpDetail;
pub const lastPortFault = impl.lastPortFault;

/// The UD mutual-probe endpoint behind the hostping_ud_* C ABI.
pub const UdEp = impl.UdEp;
pub const udOpen = impl.udOpen;
pub const udClose = impl.udClose;
pub const udSend = impl.udSend;
pub const udWait = impl.udWait;

/// Size of each registered mutual-probe buffer; the largest SEND payload.
pub const mutual_buf_bytes = types.mutual_buf_bytes;

/// expectFn fails the build unless Impl.name is a function that takes
/// exactly `params` and returns `Payload`, plain or behind an error union
/// (each implementation infers its own error set, so the sets are not
/// compared).
fn expectFn(comptime Impl: type, comptime name: []const u8, comptime params: []const type, comptime Payload: type) void {
    const where = @typeName(Impl) ++ "." ++ name;
    if (!@hasDecl(Impl, name)) @compileError("verbs: " ++ where ++ " is missing");
    const info = switch (@typeInfo(@TypeOf(@field(Impl, name)))) {
        .@"fn" => |f| f,
        else => @compileError("verbs: " ++ where ++ " is not a function"),
    };
    if (info.param_types.len != params.len) {
        @compileError(std.fmt.comptimePrint(
            "verbs: {s} takes {d} parameters, want {d}",
            .{ where, info.param_types.len, params.len },
        ));
    }
    inline for (info.param_types, params, 0..) |got, want, i| {
        if (got == null or got.? != want) {
            @compileError(std.fmt.comptimePrint(
                "verbs: {s} parameter {d} is {s}, want {s}",
                .{ where, i, if (got) |t| @typeName(t) else "generic", @typeName(want) },
            ));
        }
    }
    const ret = info.return_type.?;
    const payload = switch (@typeInfo(ret)) {
        .error_union => |eu| eu.payload,
        else => ret,
    };
    if (payload != Payload) {
        @compileError(std.fmt.comptimePrint(
            "verbs: {s} returns {s}, want {s}",
            .{ where, @typeName(payload), @typeName(Payload) },
        ));
    }
}

/// verifyImpl is the interface capi.zig calls through this file, one line
/// per function. Changing a signature in one implementation without the
/// other is a build error, not a Darwin-only surprise.
fn verifyImpl(comptime Impl: type) void {
    const str = []const u8;
    const Ep = Impl.UdEp;
    expectFn(Impl, "loopbackVerbs", &.{ str, i32, i32, str, u32, u32, u32, u32, u64 }, types.LoopResult);
    expectFn(Impl, "mutualVerbsRc", &.{ str, str, u32, u32, u32 }, types.MutualResult);
    expectFn(Impl, "mutualVerbsUd", &.{ str, str, u32, u32, u32 }, types.MutualResult);
    expectFn(Impl, "lastQpDetail", &.{}, str);
    expectFn(Impl, "lastPortFault", &.{}, bool);
    expectFn(Impl, "udOpen", &.{ str, u32 }, *Ep);
    expectFn(Impl, "udClose", &.{*Ep}, void);
    expectFn(Impl, "udSend", &.{ *Ep, *const Ep, u32 }, u64);
    expectFn(Impl, "udWait", &.{ []const ?*Ep, u64, u32, []udtag.Arrival }, usize);
}

comptime {
    verifyImpl(impl);
    // A Linux build (CI) never analyses the stub otherwise, so a signature
    // that drifted there would only fail on Darwin. The stub needs no
    // libibverbs, so it is checked here too; root_test.zig analyses its
    // bodies.
    if (builtin.os.tag == .linux) verifyImpl(@import("verbs_stub.zig"));
}
