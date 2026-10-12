const std = @import("std");

pub fn transferNs(size_bytes: u64, bw_bps: u64) u64 {
    if (bw_bps == 0) return 0x1_0000_0000;
    return (size_bytes * 8 * 1_000_000_000) / bw_bps;
}

pub fn latencyNs(tproc_ns: u64, lathost_ns: u64, size_bytes: u64, bw_bps: u64) u64 {
    return tproc_ns + lathost_ns + transferNs(size_bytes, bw_bps);
}

pub fn bandwidthBps(size_bytes: u64, lat_large_ns: u64, lat_small_ns: u64) u64 {
    if (lat_large_ns <= lat_small_ns) return 0;
    const dt = lat_large_ns - lat_small_ns;
    return (size_bytes * 8 * 1_000_000_000) / dt;
}

pub fn percentile(samples: []u64, pct: u8) u64 {
    if (samples.len == 0) return 0;
    std.mem.sort(u64, samples, {}, std.sort.asc(u64));
    const idx = @min(samples.len - 1, ((samples.len - 1) * @as(usize, pct)) / 100);
    return samples[idx];
}

// p99FromP50 is the synthetic tail the simulator reports: Eq.1 is
// deterministic, so every iteration would produce the identical sample and a
// real percentile over them is just p50. The +5% stand-in must stay identical
// to sim.World.Loopback in Go, which is the same model; TestZigSimMatchesGoSim
// in internal/zigprobe locks the two together.
pub fn p99FromP50(p50: u64) u64 {
    return p50 + p50 / 20;
}

// mutualP99FromP50 is the same idea for the mutual RTT tail, and must match
// sim.World.Mutual in Go.
pub fn mutualP99FromP50(p50: u64) u64 {
    return p50 + p50 / 4;
}

pub fn loopbackSim(small_size: u32, large_size: u32, tproc: u64, host: u64, bw: u64, out_small_p50: *u64, out_small_p99: *u64, out_large_p50: *u64, out_bw: *u64) void {
    out_small_p50.* = latencyNs(tproc, host, small_size, bw);
    out_small_p99.* = p99FromP50(out_small_p50.*);
    out_large_p50.* = latencyNs(tproc, host, large_size, bw);
    out_bw.* = bandwidthBps(large_size, out_large_p50.*, out_small_p50.*);
}

test "eq1 recovers bandwidth" {
    const size: u64 = 128 * 1024;
    const bw: u64 = 197_000_000_000;
    const small = latencyNs(400, 600, 1, bw);
    const large = latencyNs(400, 600, size, bw);
    const got = bandwidthBps(size, large, small);
    const err = @abs(@as(i64, @intCast(got)) - @as(i64, @intCast(bw)));
    try std.testing.expect(err < @as(i64, @intCast(bw / 100)));
}

test "128KiB at 200Gbps is ~5us" {
    const xfer = transferNs(128 * 1024, 200_000_000_000);
    try std.testing.expect(xfer > 4000 and xfer < 6000);
}
