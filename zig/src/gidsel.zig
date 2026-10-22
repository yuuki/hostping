//! The pure part of choosing a RoCE port's GID (verbs_linux.zig): which GIDs
//! a loopback tries first and which one a mutual probe addresses the device
//! by, decided from the 16 GID bytes alone. Kept free of ibverbs so
//! `zig build test` covers it on any host; verbs_linux.zig reads a
//! `c.ibv_gid` as a `Gid` and feeds its candidate list to the two orders.
const std = @import("std");

/// The 16 bytes of a GID, in table order.
pub const Gid = [16]u8;

pub fn isZero(g: Gid) bool {
    return std.mem.allEqual(u8, &g, 0);
}

/// ipv4Mapped reports an IPv4-mapped GID (::ffff:a.b.c.d), what a RoCEv2
/// port with an IPv4 address carries.
pub fn ipv4Mapped(g: Gid) bool {
    return std.mem.allEqual(u8, g[0..10], 0) and g[10] == 0xff and g[11] == 0xff;
}

/// linkLocal reports an fe80:: GID: the port's own link-local address, which
/// a loopback reaches without a route.
pub fn linkLocal(g: Gid) bool {
    return g[0] == 0xfe and g[1] == 0x80;
}

/// Kind is what the two orders below read of a candidate: any struct with
/// these two fields is accepted.
pub const Kind = struct {
    ipv4: bool,
    link_local: bool,
};

pub fn kindOf(g: Gid) Kind {
    return .{ .ipv4 = ipv4Mapped(g), .link_local = linkLocal(g) };
}

/// loopbackOrder writes into idx the indexes of cands in the order a
/// loopback tries them: link-local GIDs first (no route needed), then
/// IPv4-mapped ones, then the rest, each group in table order. Returns how
/// many it wrote (cands.len). IPv4-mapped destination GIDs often fail a
/// loopback with ENETUNREACH (no route to self), which is why link-local
/// goes first.
pub fn loopbackOrder(cands: anytype, idx: []usize) usize {
    var k: usize = 0;
    for (cands, 0..) |cnd, i| {
        if (cnd.link_local) {
            idx[k] = i;
            k += 1;
        }
    }
    for (cands, 0..) |cnd, i| {
        if (cnd.ipv4) {
            idx[k] = i;
            k += 1;
        }
    }
    for (cands, 0..) |cnd, i| {
        if (!cnd.link_local and !cnd.ipv4) {
            idx[k] = i;
            k += 1;
        }
    }
    return k;
}

/// preferred is the index of the GID a mutual probe addresses a device by:
/// the first IPv4-mapped one, else the first that is not link-local (a
/// routable IPv6 GID), else null (only link-local GIDs, which cannot reach
/// another device).
pub fn preferred(cands: anytype) ?usize {
    for (cands, 0..) |cnd, i| {
        if (cnd.ipv4) return i;
    }
    for (cands, 0..) |cnd, i| {
        if (!cnd.link_local) return i;
    }
    return null;
}

const t = std.testing;

fn gid(prefix: []const u8) Gid {
    var g: Gid = @splat(0);
    @memcpy(g[0..prefix.len], prefix);
    return g;
}

const zero: Gid = @splat(0);
const v4 = gid(&.{ 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 7 });
const ll = gid(&.{ 0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0x02, 0x11 });
const v6 = gid(&.{ 0x20, 0x01, 0x0d, 0xb8 });

test "a GID is read from its bytes" {
    try t.expect(isZero(zero));
    try t.expect(!isZero(v4));
    try t.expect(ipv4Mapped(v4));
    try t.expect(!ipv4Mapped(ll));
    try t.expect(!ipv4Mapped(v6));
    // ::ffff:0.0.0.0 is still IPv4-mapped, the zero GID is not.
    try t.expect(ipv4Mapped(gid(&.{ 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff })));
    try t.expect(!ipv4Mapped(zero));
    try t.expect(linkLocal(ll));
    try t.expect(!linkLocal(v4));
    try t.expect(!linkLocal(v6));
    // Only fe80:: exactly, not the whole fe80::/10.
    try t.expect(!linkLocal(gid(&.{ 0xfe, 0xa0 })));
    try t.expectEqual(Kind{ .ipv4 = true, .link_local = false }, kindOf(v4));
    try t.expectEqual(Kind{ .ipv4 = false, .link_local = true }, kindOf(ll));
    try t.expectEqual(Kind{ .ipv4 = false, .link_local = false }, kindOf(v6));
}

test "loopback tries link-local, then IPv4-mapped, then the rest, in table order" {
    const cands = [_]Kind{ kindOf(v4), kindOf(v6), kindOf(ll), kindOf(v4), kindOf(ll), kindOf(v6) };
    var idx: [cands.len]usize = undefined;
    try t.expectEqual(cands.len, loopbackOrder(&cands, &idx));
    try t.expectEqualSlices(usize, &.{ 2, 4, 0, 3, 1, 5 }, &idx);
}

test "loopback order of an empty or single-kind list is table order" {
    var idx: [4]usize = undefined;
    try t.expectEqual(@as(usize, 0), loopbackOrder(&[_]Kind{}, &idx));
    const same = [_]Kind{ kindOf(v4), kindOf(v4), kindOf(v4) };
    try t.expectEqual(@as(usize, 3), loopbackOrder(&same, &idx));
    try t.expectEqualSlices(usize, &.{ 0, 1, 2 }, idx[0..3]);
}

test "a mutual probe prefers the first IPv4-mapped GID, then the first routable one" {
    try t.expectEqual(@as(?usize, 2), preferred(&[_]Kind{ kindOf(ll), kindOf(v6), kindOf(v4), kindOf(v4) }));
    try t.expectEqual(@as(?usize, 1), preferred(&[_]Kind{ kindOf(ll), kindOf(v6), kindOf(ll) }));
    try t.expectEqual(@as(?usize, null), preferred(&[_]Kind{ kindOf(ll), kindOf(ll) }));
    try t.expectEqual(@as(?usize, null), preferred(&[_]Kind{}));
}

test "the orders accept any candidate struct with the two flags" {
    const Cand = struct { port: u8, ipv4: bool, link_local: bool };
    const cands = [_]Cand{
        .{ .port = 1, .ipv4 = false, .link_local = false },
        .{ .port = 1, .ipv4 = false, .link_local = true },
    };
    var idx: [2]usize = undefined;
    try t.expectEqual(@as(usize, 2), loopbackOrder(&cands, &idx));
    try t.expectEqualSlices(usize, &.{ 1, 0 }, &idx);
    try t.expectEqual(@as(?usize, 0), preferred(&cands));
}
