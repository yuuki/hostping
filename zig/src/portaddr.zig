//! How a probe addresses an RNIC port, decided from what ibv_query_port and
//! the GID table report. Kept free of ibverbs so `zig build test` covers it on
//! any host; verbs_linux.zig fills `Port` from the verbs and acts on `Plan`.
//!
//! A RoCE port is addressed by a RoCEv2 GID (never the fe80 RoCEv1 GID 0) and
//! that path is unchanged. A port whose link layer is InfiniBand has no RoCEv2
//! GID: it is addressed by its LID, with no GRH (`is_global = 0`).
const std = @import("std");

pub const LinkLayer = enum {
    /// IBV_LINK_LAYER_UNSPECIFIED: what older providers report for an
    /// InfiniBand port, so it is read as InfiniBand.
    unspecified,
    infiniband,
    ethernet,

    pub fn isInfiniBand(self: LinkLayer) bool {
        return self != .ethernet;
    }
};

/// What a probe needs to know about one physical port.
pub const Port = struct {
    num: u8,
    link_layer: LinkLayer,
    active: bool,
    /// The port's base LID; 0 when the subnet manager has not assigned one.
    lid: u16,
    /// Non-zero RoCEv2 GIDs in the port's GID table.
    rocev2_gids: u16,
};

/// How a probe addressed its port. The numeric values are
/// HOSTPING_ADDR_* in include/hostping.h.
pub const Mode = enum(i32) {
    /// No live probe ran (the simulator).
    none = 0,
    /// A RoCEv2 GID with a GRH (`is_global = 1`).
    rocev2_gid = 1,
    /// An InfiniBand LID, no GRH (`is_global = 0`).
    ib_lid = 2,
    /// An InfiniBand LID with a GRH over GID index 0, tried only when the
    /// LID-only RTR is refused (a subnet that requires a GRH).
    ib_lid_grh = 3,
};

/// Why a port cannot be probed at all.
pub const Skip = enum {
    /// No active port with a RoCEv2 GID and no InfiniBand port: the RoCE
    /// failure the probe always had (error.Gid).
    no_rocev2_gid,
    /// An InfiniBand port exists but none is ACTIVE (error.PortDown).
    ib_port_not_active,
    /// An InfiniBand port is ACTIVE but has LID 0: no subnet manager has
    /// configured it (error.NoLid).
    ib_no_lid,
};

pub const Plan = union(enum) {
    /// Address the device by RoCEv2 GIDs over every active port, exactly as
    /// before InfiniBand ports were supported.
    rocev2,
    /// Address the device by this InfiniBand port's LID.
    ib: struct { port: u8, lid: u16 },
    skip: Skip,
};

/// Unicast LIDs are 0x0001..0xbfff; 0 is unassigned and 0xc000.. multicast.
fn unicastLid(lid: u16) bool {
    return lid != 0 and lid < 0xc000;
}

/// plan picks how to address a device from its ports, in port order. RoCE
/// wins whenever any active non-InfiniBand port has a RoCEv2 GID, so a RoCE
/// device is probed exactly as before. Otherwise the first ACTIVE InfiniBand
/// port with a unicast LID is used. When none is usable the skip names the
/// InfiniBand cause if there is an InfiniBand port at all.
pub fn plan(ports: []const Port) Plan {
    for (ports) |p| {
        if (p.active and !p.link_layer.isInfiniBand() and p.rocev2_gids > 0) return .rocev2;
    }
    for (ports) |p| {
        if (p.active and p.link_layer.isInfiniBand() and unicastLid(p.lid)) {
            return .{ .ib = .{ .port = p.num, .lid = p.lid } };
        }
    }
    var ib_any = false;
    for (ports) |p| {
        if (!p.link_layer.isInfiniBand()) continue;
        if (p.active) return .{ .skip = .ib_no_lid };
        ib_any = true;
    }
    if (ib_any) return .{ .skip = .ib_port_not_active };
    return .{ .skip = .no_rocev2_gid };
}

/// skipError is the probe error for a skip; errclass.verbsRC maps every one
/// of them to HOSTPING_ERR_PORT.
pub fn skipError(s: Skip) anyerror {
    return switch (s) {
        .no_rocev2_gid => error.Gid,
        .ib_port_not_active => error.PortDown,
        .ib_no_lid => error.NoLid,
    };
}

/// ibv_wc_status values the GRH retry looks at; verbs_linux.zig checks them
/// against <infiniband/verbs.h> at compile time.
pub const wc_bad_resp_err: u32 = 7;
pub const wc_rem_inv_req_err: u32 = 9;
pub const wc_rem_access_err: u32 = 10;
pub const wc_rem_op_err: u32 = 11;
pub const wc_retry_exc_err: u32 = 12;
pub const wc_rnr_retry_exc_err: u32 = 13;
pub const wc_resp_timeout_err: u32 = 20;

/// retryOtherMode reports whether a loopback WRITE that completed with
/// `status` on an InfiniBand port is worth one more try with the other
/// addressing mode (LID alone, or LID plus a GRH over GID index 0). Only the
/// errors that say the request left the port and no good answer came back
/// qualify: the retry counter ran out, or the responder refused or answered
/// badly. Local errors (a bad MR, a flushed WR) would fail the same way again.
pub fn retryOtherMode(status: u32) bool {
    return switch (status) {
        wc_bad_resp_err,
        wc_rem_inv_req_err,
        wc_rem_access_err,
        wc_rem_op_err,
        wc_retry_exc_err,
        wc_rnr_retry_exc_err,
        wc_resp_timeout_err,
        => true,
        else => false,
    };
}

/// portFault reports whether a loopback that failed on an InfiniBand port
/// failed at the port itself: its last WRITE, in the last addressing mode
/// the probe tried, completed with one of the errors retryOtherMode retries
/// on. The loopback only gives up on such an error once the other mode has
/// been tried too (or could not connect, or does not exist), so it means no
/// mode made the port answer its own WRITE. Every other probe of the same
/// port would wait out the same retry budget, which is what lets the caller
/// stop probing the port for the rest of its run. RoCE ports never qualify.
pub fn portFault(ib: bool, wc_failed: bool, status: u32) bool {
    return ib and wc_failed and retryOtherMode(status);
}

/// ibOrder is the order an InfiniBand loopback tries its addressing modes:
/// the mode that last worked on the port first, LID alone otherwise.
pub fn ibOrder(cached: ?Mode) [2]Mode {
    if (cached) |m| {
        if (m == .ib_lid_grh) return .{ .ib_lid_grh, .ib_lid };
    }
    return .{ .ib_lid, .ib_lid_grh };
}

/// ModeCache remembers, per device and port, the InfiniBand addressing mode
/// whose loopback last completed, so later probes start with it instead of
/// paying for a failed attempt every time. It holds a fixed number of ports;
/// past that the oldest entry is replaced. Not thread-safe: the caller locks.
pub const ModeCache = struct {
    pub const capacity = 64;
    const name_len = 64;

    const Entry = struct {
        name: [name_len]u8,
        len: u8,
        port: u8,
        mode: Mode,
    };

    entries: [capacity]Entry = undefined,
    n: usize = 0,
    next: usize = 0,

    fn find(self: *const ModeCache, name: []const u8, port: u8) ?usize {
        if (name.len > name_len) return null;
        for (self.entries[0..self.n], 0..) |e, i| {
            if (e.port == port and std.mem.eql(u8, e.name[0..e.len], name)) return i;
        }
        return null;
    }

    pub fn get(self: *const ModeCache, name: []const u8, port: u8) ?Mode {
        const i = self.find(name, port) orelse return null;
        return self.entries[i].mode;
    }

    pub fn put(self: *ModeCache, name: []const u8, port: u8, mode: Mode) void {
        if (name.len > name_len) return;
        if (self.find(name, port)) |i| {
            self.entries[i].mode = mode;
            return;
        }
        var i = self.n;
        if (self.n < capacity) {
            self.n += 1;
        } else {
            i = self.next;
            self.next = (self.next + 1) % capacity;
        }
        self.entries[i] = .{ .name = undefined, .len = @intCast(name.len), .port = port, .mode = mode };
        @memcpy(self.entries[i].name[0..name.len], name);
    }
};

const t = std.testing;

fn mkPort(num: u8, ll: LinkLayer, active: bool, lid: u16, gids: u16) Port {
    return .{ .num = num, .link_layer = ll, .active = active, .lid = lid, .rocev2_gids = gids };
}

test "a RoCE port with a RoCEv2 GID keeps GID addressing" {
    try t.expectEqual(Plan.rocev2, plan(&.{mkPort(1, .ethernet, true, 0, 2)}));
}

test "a RoCE port without a RoCEv2 GID is the old no-GID skip" {
    try t.expectEqual(Plan{ .skip = .no_rocev2_gid }, plan(&.{mkPort(1, .ethernet, true, 0, 0)}));
    try t.expectEqual(Plan{ .skip = .no_rocev2_gid }, plan(&.{mkPort(1, .ethernet, false, 0, 2)}));
    try t.expectEqual(Plan{ .skip = .no_rocev2_gid }, plan(&.{}));
}

test "an active InfiniBand port with a LID is addressed by LID" {
    try t.expectEqual(Plan{ .ib = .{ .port = 1, .lid = 7 } }, plan(&.{mkPort(1, .infiniband, true, 7, 0)}));
    // Older providers report the link layer of an IB port as unspecified.
    try t.expectEqual(Plan{ .ib = .{ .port = 1, .lid = 7 } }, plan(&.{mkPort(1, .unspecified, true, 7, 0)}));
}

test "InfiniBand skips name their cause" {
    try t.expectEqual(Plan{ .skip = .ib_no_lid }, plan(&.{mkPort(1, .infiniband, true, 0, 0)}));
    try t.expectEqual(Plan{ .skip = .ib_port_not_active }, plan(&.{mkPort(1, .infiniband, false, 7, 0)}));
    // A multicast LID is no port address.
    try t.expectEqual(Plan{ .skip = .ib_no_lid }, plan(&.{mkPort(1, .infiniband, true, 0xc001, 0)}));
    try t.expectEqual(error.NoLid, skipError(.ib_no_lid));
    try t.expectEqual(error.PortDown, skipError(.ib_port_not_active));
    try t.expectEqual(error.Gid, skipError(.no_rocev2_gid));
}

test "the first usable InfiniBand port wins" {
    const ports = [_]Port{ mkPort(1, .infiniband, false, 3, 0), mkPort(2, .infiniband, true, 9, 0) };
    try t.expectEqual(Plan{ .ib = .{ .port = 2, .lid = 9 } }, plan(&ports));
}

test "RoCE wins over InfiniBand on a mixed device" {
    const ports = [_]Port{ mkPort(1, .infiniband, true, 3, 0), mkPort(2, .ethernet, true, 0, 1) };
    try t.expectEqual(Plan.rocev2, plan(&ports));
}

test "a remote completion error retries with the other mode, a local one does not" {
    try t.expect(retryOtherMode(wc_retry_exc_err));
    try t.expect(retryOtherMode(wc_rem_access_err));
    try t.expect(retryOtherMode(wc_resp_timeout_err));
    try t.expect(!retryOtherMode(0)); // success
    try t.expect(!retryOtherMode(4)); // local protection error
    try t.expect(!retryOtherMode(5)); // flushed
}

test "only a port-level completion error on InfiniBand is a port fault" {
    try t.expect(portFault(true, true, wc_retry_exc_err));
    try t.expect(portFault(true, true, wc_resp_timeout_err));
    try t.expect(!portFault(false, true, wc_retry_exc_err)); // RoCE
    try t.expect(!portFault(true, false, wc_retry_exc_err)); // not a completion error
    try t.expect(!portFault(true, true, 4)); // local protection error
    try t.expect(!portFault(true, true, 5)); // flushed
}

test "the cached mode is tried first" {
    try t.expectEqual([2]Mode{ .ib_lid, .ib_lid_grh }, ibOrder(null));
    try t.expectEqual([2]Mode{ .ib_lid, .ib_lid_grh }, ibOrder(.ib_lid));
    try t.expectEqual([2]Mode{ .ib_lid_grh, .ib_lid }, ibOrder(.ib_lid_grh));
}

test "mode cache is per device and port" {
    var mc: ModeCache = .{};
    try t.expectEqual(@as(?Mode, null), mc.get("dev0", 1));
    mc.put("dev0", 1, .ib_lid_grh);
    mc.put("dev1", 1, .ib_lid);
    try t.expectEqual(@as(?Mode, .ib_lid_grh), mc.get("dev0", 1));
    try t.expectEqual(@as(?Mode, .ib_lid), mc.get("dev1", 1));
    try t.expectEqual(@as(?Mode, null), mc.get("dev0", 2));
    mc.put("dev0", 1, .ib_lid);
    try t.expectEqual(@as(?Mode, .ib_lid), mc.get("dev0", 1));
    try t.expectEqual(@as(usize, 2), mc.n);
}

test "a full mode cache replaces its oldest entry" {
    var mc: ModeCache = .{};
    var buf: [8]u8 = undefined;
    var i: usize = 0;
    while (i < ModeCache.capacity + 1) : (i += 1) {
        const name = std.fmt.bufPrint(&buf, "d{d}", .{i}) catch unreachable;
        mc.put(name, 1, .ib_lid_grh);
    }
    try t.expectEqual(@as(usize, ModeCache.capacity), mc.n);
    try t.expectEqual(@as(?Mode, null), mc.get("d0", 1));
    try t.expectEqual(@as(?Mode, .ib_lid_grh), mc.get("d64", 1));
}
