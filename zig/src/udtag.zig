//! Pure pieces of the UD mutual probe (verbs_linux.zig): the immediate-data
//! tag every probe carries and the receive-slot layout. Kept free of ibverbs
//! so `zig build test` covers them on any host.
const std = @import("std");

/// Every UD receive lands behind a 40-byte Global Routing Header, which RoCE
/// always carries; a receive buffer shorter than GRH + payload completes in
/// error.
pub const grh_bytes: u32 = 40;

/// Endpoints one probe session can address. The requester's index travels in
/// seven bits of the tag so the receiver can answer without a lookup.
pub const max_endpoints: u32 = 128;

/// The probe sequence number is the tag's top 24 bits.
pub const seq_mask: u32 = 0x00ff_ffff;

/// One reply as the probe hands it back: the requester's endpoint index, the
/// probe's sequence number and when the reply was polled (CLOCK_MONOTONIC ns).
pub const Arrival = struct {
    ep: u32,
    seq: u32,
    t_ns: u64,
};

/// request is the tag a probe's request carries: seq << 8 | src << 1 | 0.
pub fn request(seq: u32, src: u32) u32 {
    return ((seq & seq_mask) << 8) | ((src & (max_endpoints - 1)) << 1);
}

/// reply is the tag the answer to `tag` carries: the same tag with bit 0 set,
/// so the requester can match it to the probe it sent and to nothing else.
pub fn reply(tag: u32) u32 {
    return tag | 1;
}

pub fn isReply(tag: u32) bool {
    return tag & 1 == 1;
}

pub fn srcOf(tag: u32) u32 {
    return (tag >> 1) & (max_endpoints - 1);
}

pub fn seqOf(tag: u32) u32 {
    return tag >> 8;
}

/// slotBytes is one receive slot: GRH + payload, rounded up to a cache line so
/// neighbouring slots never share one.
pub fn slotBytes(msg: u32) usize {
    return std.mem.alignForward(usize, @as(usize, grh_bytes) + @as(usize, msg), 64);
}

/// bufBytes is the registered buffer of one UD endpoint: recv_depth receive
/// slots plus one send slot, in whole pages (aligned_alloc wants a multiple
/// of the alignment).
pub fn bufBytes(slot: usize, recv_depth: u32) usize {
    return std.mem.alignForward(usize, slot * (@as(usize, recv_depth) + 1), 4096);
}

/// mtuBytes converts an ibv_mtu enum value (IBV_MTU_256 = 1 … IBV_MTU_4096 = 5)
/// to bytes. A UD message cannot exceed the path MTU; 0 means unknown.
pub fn mtuBytes(mtu: u32) u32 {
    if (mtu < 1 or mtu > 5) return 0;
    return @as(u32, 128) << @as(u5, @intCast(mtu));
}

test "request and reply tags round-trip seq and src" {
    const t = request(0x12_3456, 77);
    try std.testing.expect(!isReply(t));
    try std.testing.expectEqual(@as(u32, 0x12_3456), seqOf(t));
    try std.testing.expectEqual(@as(u32, 77), srcOf(t));
    const r = reply(t);
    try std.testing.expect(isReply(r));
    try std.testing.expectEqual(seqOf(t), seqOf(r));
    try std.testing.expectEqual(srcOf(t), srcOf(r));
}

test "seq wraps at 24 bits instead of clobbering src" {
    const t = request(0x0100_0005, 3);
    try std.testing.expectEqual(@as(u32, 5), seqOf(t));
    try std.testing.expectEqual(@as(u32, 3), srcOf(t));
}

test "slot holds the GRH and the payload" {
    try std.testing.expectEqual(@as(usize, 64), slotBytes(1));
    try std.testing.expectEqual(@as(usize, 4160), slotBytes(4096));
    try std.testing.expect(slotBytes(24) >= grh_bytes + 24);
}

test "the endpoint buffer holds every receive slot and the send slot, in whole pages" {
    try std.testing.expectEqual(@as(usize, 4096), bufBytes(64, 1));
    try std.testing.expectEqual(@as(usize, 8192), bufBytes(64, 127));
    const slot = slotBytes(4096);
    const n = bufBytes(slot, 128);
    try std.testing.expectEqual(@as(usize, 0), n % 4096);
    try std.testing.expect(n >= slot * 129);
    try std.testing.expect(n - slot * 129 < 4096);
}

test "mtu enum to bytes" {
    try std.testing.expectEqual(@as(u32, 256), mtuBytes(1));
    try std.testing.expectEqual(@as(u32, 1024), mtuBytes(3));
    try std.testing.expectEqual(@as(u32, 4096), mtuBytes(5));
    try std.testing.expectEqual(@as(u32, 0), mtuBytes(0));
}
