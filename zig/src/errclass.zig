const std = @import("std");

/// Connectivity-class errors mean the RoCE path between the two endpoints did
/// not come up, or came up but carried no completion. Everything else (missing
/// device, no usable GID or LID, PD/CQ/QP/MR allocation, bad arguments) is a local
/// setup failure that says nothing about the path, so hostping_mutual must not
/// report it as a measured "not connected" result.
pub fn isConnectivity(err: anyerror) bool {
    return switch (err) {
        error.Connect, error.Rtr, error.Rts, error.Timeout, error.Wc, error.Post, error.Poll => true,
        else => false,
    };
}

/// verbsRC maps a probe error to the HOSTPING_ERR_* code in include/hostping.h.
/// Connectivity-class errors always map to HOSTPING_ERR_TIMEOUT (4) or
/// HOSTPING_ERR_CONNECT (5); nothing else does.
pub fn verbsRC(err: anyerror) i32 {
    return switch (err) {
        error.NoDevice => 2,
        error.Timeout => 4,
        error.NoGDR => 6,
        error.Gid, error.NoLid, error.PortDown, error.Port, error.Device => 7,
        else => if (isConnectivity(err)) 5 else 3,
    };
}

test "connectivity class is the handshake, completion and timeout failures" {
    const conn = [_]anyerror{ error.Connect, error.Rtr, error.Rts, error.Timeout, error.Wc, error.Post, error.Poll };
    for (conn) |e| {
        try std.testing.expect(isConnectivity(e));
        const rc = verbsRC(e);
        try std.testing.expect(rc == 4 or rc == 5);
    }
}

test "setup failures are not connectivity class" {
    const setup = [_]anyerror{ error.NoDevice, error.Gid, error.Device, error.Port, error.Pd, error.Cq, error.Qp, error.Mr, error.Init, error.Alloc, error.Numa, error.NoGDR, error.Clock, error.NoLid, error.PortDown };
    for (setup) |e| {
        try std.testing.expect(!isConnectivity(e));
        const rc = verbsRC(e);
        try std.testing.expect(rc != 4 and rc != 5);
    }
}

test "verbsRC keeps the documented codes" {
    try std.testing.expectEqual(@as(i32, 2), verbsRC(error.NoDevice));
    try std.testing.expectEqual(@as(i32, 4), verbsRC(error.Timeout));
    try std.testing.expectEqual(@as(i32, 5), verbsRC(error.Connect));
    try std.testing.expectEqual(@as(i32, 6), verbsRC(error.NoGDR));
    try std.testing.expectEqual(@as(i32, 7), verbsRC(error.Gid));
    try std.testing.expectEqual(@as(i32, 7), verbsRC(error.NoLid));
    try std.testing.expectEqual(@as(i32, 7), verbsRC(error.PortDown));
    try std.testing.expectEqual(@as(i32, 3), verbsRC(error.Pd));
}
