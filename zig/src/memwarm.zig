//! Optional memory warm-up before a host-memory loopback probe (--mem-warmup).
//!
//! On a host whose uncore/memory power management is dynamic, an idle memory
//! subsystem can sit at a lower operating point than a loaded one, and the
//! loopback's own traffic is too short to move it reliably, so a host-memory
//! path reads at different levels from run to run. The warm-up writes the
//! probe's own NUMA-bound buffers from the CPU for a bounded time right before
//! the timed iterations, so the measurement starts with the node's memory
//! carrying traffic.
//!
//! The probe's buffers are small enough to stay in the CPU caches, where plain
//! stores never reach DRAM. The fill therefore uses non-temporal stores on
//! x86_64 (MOVNTI) and aarch64 (STNP), which bypass the caches and put the
//! writes on the memory channels of the node the buffers are bound to. Other
//! architectures fall back to plain stores.
const std = @import("std");
const builtin = @import("builtin");
const sysio = @import("sysio.zig");

/// Upper bound on one warm-up, whatever the caller asks for. The Go side
/// rejects larger values; this is the probe's own guard.
pub const max_ns: u64 = 1_000_000_000;

/// Bytes written between two clock reads: small enough that the deadline is
/// overshot by microseconds, large enough that the clock read is noise.
pub const chunk_bytes: usize = 64 * 1024;

/// CLOCK_MONOTONIC (std.Io's `awake` clock), the clock the probe's other
/// timings use.
pub const SystemClock = struct {
    pub fn now(_: SystemClock) !u64 {
        const ns = std.Io.Clock.now(.awake, sysio.io()).toNanoseconds();
        if (ns < 0) return error.Clock;
        return @intCast(ns);
    }
};

/// Writes every buffer in `bufs`, round after round, until `budget_ns`
/// (capped at max_ns) has elapsed on `clock`, and returns the time actually
/// spent. `clock` is any value with `fn now(self) !u64` returning monotonic
/// nanoseconds; a failing or backward clock ends the warm-up early. A zero
/// budget, or no bytes to write, does nothing and returns 0.
pub fn run(bufs: []const []u8, budget_ns: u64, clock: anytype) u64 {
    const budget = @min(budget_ns, max_ns);
    if (budget == 0) return 0;
    var total: usize = 0;
    for (bufs) |b| total += b.len;
    if (total == 0) return 0;

    const t0 = clock.now() catch return 0;
    var now = t0;
    var round: u8 = 0;
    outer: while (true) : (round +%= 1) {
        // A different byte each round, so no store is a rewrite of the same
        // value that a write-combining or zero-detecting path could drop.
        const fill: u8 = 0x5a ^ round;
        for (bufs) |buf| {
            var off: usize = 0;
            while (off < buf.len) {
                const end = @min(off + chunk_bytes, buf.len);
                streamFill(buf[off..end], fill);
                off = end;
                now = clock.now() catch break :outer;
                if (now < t0 or now - t0 >= budget) break :outer;
            }
        }
    }
    storeFence();
    return if (now >= t0) now - t0 else 0;
}

fn streamFill(buf: []u8, fill: u8) void {
    const word: u64 = @as(u64, fill) *% 0x0101_0101_0101_0101;
    switch (builtin.cpu.arch) {
        .x86_64 => {
            const head = std.mem.alignForward(usize, @intFromPtr(buf.ptr), 8) - @intFromPtr(buf.ptr);
            if (head >= buf.len) return @memset(buf, fill);
            @memset(buf[0..head], fill);
            const words = (buf.len - head) / 8;
            const base: [*]u64 = @ptrCast(@alignCast(buf.ptr + head));
            var i: usize = 0;
            while (i < words) : (i += 1) {
                asm volatile ("movntiq %[v], (%[p])"
                    :
                    : [p] "r" (base + i),
                      [v] "r" (word),
                    : .{ .memory = true });
            }
            @memset(buf[head + words * 8 ..], fill);
        },
        .aarch64 => {
            const head = std.mem.alignForward(usize, @intFromPtr(buf.ptr), 16) - @intFromPtr(buf.ptr);
            if (head >= buf.len) return @memset(buf, fill);
            @memset(buf[0..head], fill);
            const pairs = (buf.len - head) / 16;
            const base: [*]u8 = buf.ptr + head;
            var i: usize = 0;
            while (i < pairs) : (i += 1) {
                asm volatile ("stnp %[a], %[b], [%[p]]"
                    :
                    : [p] "r" (base + i * 16),
                      [a] "r" (word),
                      [b] "r" (word),
                    : .{ .memory = true });
            }
            @memset(buf[head + pairs * 16 ..], fill);
        },
        else => {
            @memset(buf, fill);
            std.mem.doNotOptimizeAway(buf.ptr);
        },
    }
}

/// Orders the non-temporal stores before anything the probe does next.
fn storeFence() void {
    switch (builtin.cpu.arch) {
        .x86_64 => asm volatile ("sfence" ::: .{ .memory = true }),
        .aarch64 => asm volatile ("dmb ish" ::: .{ .memory = true }),
        else => {},
    }
}

/// A clock that advances by `step` on every read, so a test can count reads
/// against the budget without depending on how fast the machine is.
const StepClock = struct {
    t: *u64,
    step: u64,
    reads: *usize,
    fail_after: usize = std.math.maxInt(usize),

    pub fn now(self: StepClock) !u64 {
        if (self.reads.* >= self.fail_after) return error.Clock;
        self.reads.* += 1;
        const v = self.t.*;
        self.t.* += self.step;
        return v;
    }
};

test "zero budget does nothing" {
    var buf: [4096]u8 = @splat(0);
    var t: u64 = 0;
    var reads: usize = 0;
    const bufs = [_][]u8{&buf};
    try std.testing.expectEqual(@as(u64, 0), run(&bufs, 0, StepClock{ .t = &t, .step = 1, .reads = &reads }));
    try std.testing.expectEqual(@as(usize, 0), reads);
    try std.testing.expectEqual(@as(u8, 0), buf[0]);
}

test "stops at the deadline and writes every buffer" {
    var a: [3 * chunk_bytes]u8 align(4096) = undefined;
    var b: [3 * chunk_bytes]u8 align(4096) = undefined;
    @memset(&a, 0);
    @memset(&b, 0);
    var t: u64 = 1000;
    var reads: usize = 0;
    const bufs = [_][]u8{ &a, &b };
    // 10 chunks of budget at 1 per read: the first round covers both
    // buffers (6 chunks), the second stops after 4 more.
    const spent = run(&bufs, 10, StepClock{ .t = &t, .step = 1, .reads = &reads });
    try std.testing.expectEqual(@as(u64, 10), spent);
    try std.testing.expectEqual(@as(usize, 11), reads); // t0 + one per chunk
    for (a) |x| try std.testing.expect(x != 0);
    for (b) |x| try std.testing.expect(x != 0);
}

test "budget is capped at max_ns" {
    var buf: [chunk_bytes]u8 align(16) = undefined;
    var t: u64 = 0;
    var reads: usize = 0;
    const bufs = [_][]u8{&buf};
    const step = max_ns / 4;
    const spent = run(&bufs, 60 * max_ns, StepClock{ .t = &t, .step = step, .reads = &reads });
    try std.testing.expectEqual(max_ns, spent);
    try std.testing.expectEqual(@as(usize, 5), reads);
}

test "a failing clock ends the warm-up" {
    var buf: [chunk_bytes]u8 align(16) = undefined;
    var t: u64 = 0;
    var reads: usize = 0;
    const bufs = [_][]u8{&buf};
    const spent = run(&bufs, max_ns, StepClock{ .t = &t, .step = 1, .reads = &reads, .fail_after = 3 });
    try std.testing.expectEqual(@as(usize, 3), reads);
    try std.testing.expectEqual(@as(u64, 2), spent);
}

test "unaligned and short buffers are filled completely" {
    var raw: [4096 + 37]u8 align(16) = undefined;
    @memset(&raw, 0);
    var t: u64 = 0;
    var reads: usize = 0;
    const bufs = [_][]u8{ raw[3 .. 3 + 4096 + 30], raw[0..2] };
    _ = run(&bufs, 2, StepClock{ .t = &t, .step = 1, .reads = &reads });
    for (raw[3 .. 3 + 4096 + 30]) |x| try std.testing.expect(x != 0);
    try std.testing.expect(raw[0] != 0 and raw[1] != 0);
    try std.testing.expectEqual(@as(u8, 0), raw[2]);
    try std.testing.expectEqual(@as(u8, 0), raw[raw.len - 1]);
}

test "real clock: a short warm-up ends on time" {
    var buf: [4 * chunk_bytes]u8 align(4096) = undefined;
    const bufs = [_][]u8{&buf};
    const budget: u64 = 2_000_000; // 2 ms
    const clk = SystemClock{};
    const w0 = try clk.now();
    const spent = run(&bufs, budget, clk);
    const wall = (try clk.now()) - w0;
    try std.testing.expect(spent >= budget);
    // Overshoot is one chunk plus a clock read; allow a generous margin for
    // a loaded CI machine.
    try std.testing.expect(wall < budget + 200_000_000);
}
