//! The one-line reason behind a probe error. A probe returns a bare Zig
//! error (error.Rtr, error.NoGDR, ...); the detail is what it saw when it
//! failed, "errno=113 (EHOSTUNREACH); after 3 gid(s)" or "missing gpu", and
//! capi.zig's setVerbsErr reports "{error}: {detail}" to the caller. Each
//! implementation keeps one Detail (verbs_linux.zig, verbs_stub.zig) and
//! gdr.zig keeps its own for the allocation step.
const std = @import("std");

/// Bytes of the buffer, the NUL terminator included.
pub const len = 96;

pub const Detail = struct {
    buf: [len]u8 = @splat(0),

    /// The stored reason; empty when nothing was stored since the last clear.
    pub fn get(self: *const Detail) []const u8 {
        return std.mem.sliceTo(&self.buf, 0);
    }

    pub fn clear(self: *Detail) void {
        @memset(&self.buf, 0);
    }

    /// set replaces the reason with the formatted text. Text that does not
    /// fit is cut at the buffer's end.
    pub fn set(self: *Detail, comptime fmt: []const u8, args: anytype) void {
        self.clear();
        _ = std.fmt.bufPrint(&self.buf, fmt, args) catch {};
    }

    /// append adds the formatted text after the stored reason, "; "
    /// between them, or stores it alone when nothing is stored.
    pub fn append(self: *Detail, comptime fmt: []const u8, args: anytype) void {
        const old = self.save();
        const head = old.get();
        if (head.len == 0) {
            self.set(fmt, args);
            return;
        }
        self.set("{s}; " ++ fmt, .{head} ++ args);
    }

    /// save copies the reason out, for a restore after an attempt that may
    /// overwrite it.
    pub fn save(self: *const Detail) Detail {
        return self.*;
    }

    pub fn restore(self: *Detail, saved: Detail) void {
        self.* = saved;
    }

    /// setErrno stores a failed libc or verbs call's errno, "errno=113
    /// (EHOSTUNREACH)" for the values a probe meets most often, "errno=7"
    /// for any other.
    pub fn setErrno(self: *Detail, n: i32) void {
        const tag = errnoName(n);
        if (tag.len > 0) {
            self.set("errno={d} ({s})", .{ n, tag });
        } else {
            self.set("errno={d}", .{n});
        }
    }
};

/// errnoName names the errno values a probe's address and QP calls fail
/// with most often: EINVAL (a refused QP attribute), ENETUNREACH and
/// EHOSTUNREACH (no route to the peer's GID). Anything else is "".
pub fn errnoName(n: i32) []const u8 {
    return switch (n) {
        22 => "EINVAL",
        101 => "ENETUNREACH",
        113 => "EHOSTUNREACH",
        else => "",
    };
}

test "a fresh detail is empty and set formats into it" {
    var d: Detail = .{};
    try std.testing.expectEqualStrings("", d.get());
    d.set("errno={d} ({s})", .{ 113, "EHOSTUNREACH" });
    try std.testing.expectEqualStrings("errno=113 (EHOSTUNREACH)", d.get());
    d.set("{s}", .{"missing gpu"});
    try std.testing.expectEqualStrings("missing gpu", d.get());
    d.clear();
    try std.testing.expectEqualStrings("", d.get());
}

test "set replaces the whole reason, a shorter one leaves no tail" {
    var d: Detail = .{};
    d.set("{s}", .{"transport retry counter exceeded (12)"});
    d.set("{s}", .{"reg_mr"});
    try std.testing.expectEqualStrings("reg_mr", d.get());
}

test "append joins with a semicolon or stands alone" {
    var d: Detail = .{};
    d.append("after {d} {s}", .{ 3, "gid(s)" });
    try std.testing.expectEqualStrings("after 3 gid(s)", d.get());
    d.set("errno={d}", .{101});
    d.append("after {d} {s}", .{ 2, "lid attempt(s)" });
    try std.testing.expectEqualStrings("errno=101; after 2 lid attempt(s)", d.get());
}

test "save and restore bring back the reason an attempt overwrote" {
    var d: Detail = .{};
    d.set("{s}", .{"remote operation error (14)"});
    const saved = d.save();
    d.set("errno={d}", .{22});
    try std.testing.expectEqualStrings("errno=22", d.get());
    d.restore(saved);
    try std.testing.expectEqualStrings("remote operation error (14)", d.get());
}

test "text longer than the buffer is cut, never overrun" {
    var d: Detail = .{};
    const long: [2 * len]u8 = @splat('x');
    d.set("{s}", .{&long});
    try std.testing.expect(d.get().len <= len);
    try std.testing.expect(d.get().len > 0);
}

test "errno is stored with its name when it has one" {
    try std.testing.expectEqualStrings("EINVAL", errnoName(22));
    try std.testing.expectEqualStrings("ENETUNREACH", errnoName(101));
    try std.testing.expectEqualStrings("EHOSTUNREACH", errnoName(113));
    try std.testing.expectEqualStrings("", errnoName(7));
    var d: Detail = .{};
    d.setErrno(113);
    try std.testing.expectEqualStrings("errno=113 (EHOSTUNREACH)", d.get());
    d.setErrno(7);
    try std.testing.expectEqualStrings("errno=7", d.get());
}
