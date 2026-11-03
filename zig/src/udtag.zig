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

/// acceptRequest reports whether a request tagged `tag` that reached endpoint
/// `self` from QP number `src_qp` was sent by the requester the tag names.
/// qpns holds each endpoint's QP number (null when it is not open).
pub fn acceptRequest(qpns: []const ?u32, self: usize, tag: u32, src_qp: u32) bool {
    const from = srcOf(tag);
    if (from == self or from >= qpns.len) return false;
    const q = qpns[from] orelse return false;
    return q == src_qp;
}

/// acceptReply reports whether a reply tagged `tag` that reached endpoint
/// `self` from QP number `src_qp` answers one of self's requests and came from
/// another endpoint. QP numbers are allocated per device, so two endpoints may
/// share one; this asks whether any other endpoint has it, never which.
pub fn acceptReply(qpns: []const ?u32, self: usize, tag: u32, src_qp: u32) bool {
    if (srcOf(tag) != self) return false;
    for (qpns, 0..) |maybe, i| {
        if (i == self) continue;
        if (maybe) |q| {
            if (q == src_qp) return true;
        }
    }
    return false;
}

test "requests are accepted only from the QP of the endpoint the tag names" {
    const qpns = [_]?u32{ 17, 42, null };
    try std.testing.expect(acceptRequest(&qpns, 0, request(5, 1), 42));
    try std.testing.expect(!acceptRequest(&qpns, 0, request(5, 1), 17)); // wrong QP
    try std.testing.expect(!acceptRequest(&qpns, 0, request(5, 1), 99)); // foreign QP
    try std.testing.expect(!acceptRequest(&qpns, 0, request(5, 0), 17)); // from itself
    try std.testing.expect(!acceptRequest(&qpns, 0, request(5, 2), 0)); // closed endpoint
    try std.testing.expect(!acceptRequest(&qpns, 0, request(5, 9), 42)); // out of range
}

test "replies are accepted only for own requests and from another endpoint" {
    const qpns = [_]?u32{ 17, 42 };
    try std.testing.expect(acceptReply(&qpns, 0, reply(request(5, 0)), 42));
    try std.testing.expect(!acceptReply(&qpns, 0, reply(request(5, 1)), 42)); // not ours
    try std.testing.expect(!acceptReply(&qpns, 0, reply(request(5, 0)), 99)); // foreign QP
    try std.testing.expect(!acceptReply(&qpns, 0, reply(request(5, 0)), 17)); // own QP only
}

test "endpoints on different devices may share a QP number" {
    // QP numbers are per device: the first UD QP on two RNICs can both be 17.
    const qpns = [_]?u32{ 17, 17 };
    try std.testing.expect(acceptRequest(&qpns, 1, request(3, 0), 17));
    try std.testing.expect(acceptRequest(&qpns, 0, request(3, 1), 17));
    try std.testing.expect(acceptReply(&qpns, 0, reply(request(3, 0)), 17));
    try std.testing.expect(acceptReply(&qpns, 1, reply(request(3, 1)), 17));
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
