const std = @import("std");

/// Where the loopback WRITE reads from and writes to. NSDI'23 §4.1.1
/// registers both regions in the endpoint under test, so the measured data
/// path is endpoint → RNIC → endpoint. For a memory endpoint that is two
/// NUMA-bound host DRAM buffers; for a GPU endpoint it is one device
/// allocation split into a source half and a destination half, registered as
/// a single MR (one dma-buf fd). Host DRAM never stands in for the GPU side.
pub const Plan = struct {
    /// Bytes behind each region; every WRITE fits inside one region.
    region_len: usize,
    /// Bytes of one allocation. Host: one per region. GPU: both regions.
    alloc_len: usize,
    /// Byte offset of the source region inside its allocation.
    src_off: usize,
    /// Byte offset of the destination region inside its allocation.
    dst_off: usize,
};

pub const host_align: usize = 4096;
/// GPUDirect RDMA pins device memory in 64 KiB pages; keep each half on a
/// page boundary so the destination never shares a page with the source.
pub const gpu_align: usize = 65536;

fn alignUp(n: usize, a: usize) usize {
    return (n + a - 1) & ~(a - 1);
}

pub fn plan(large_size: u32, gpu: bool) Plan {
    const bytes = @max(@as(usize, large_size), host_align);
    if (!gpu) {
        const len = alignUp(bytes, host_align);
        return .{ .region_len = len, .alloc_len = len, .src_off = 0, .dst_off = 0 };
    }
    const len = alignUp(bytes, gpu_align);
    return .{ .region_len = len, .alloc_len = 2 * len, .src_off = 0, .dst_off = len };
}

test "host plan keeps two separate page-aligned buffers" {
    const p = plan(128 * 1024, false);
    try std.testing.expectEqual(@as(usize, 128 * 1024), p.region_len);
    try std.testing.expectEqual(p.region_len, p.alloc_len);
    try std.testing.expectEqual(@as(usize, 0), p.src_off);
    try std.testing.expectEqual(@as(usize, 0), p.dst_off);
}

test "host plan rounds small sizes up to one page" {
    const p = plan(1, false);
    try std.testing.expectEqual(@as(usize, 4096), p.region_len);
    try std.testing.expectEqual(@as(usize, 4096), p.alloc_len);
}

test "gpu plan puts source and destination in one device allocation" {
    const p = plan(128 * 1024, true);
    try std.testing.expectEqual(@as(usize, 128 * 1024), p.region_len);
    try std.testing.expectEqual(@as(usize, 256 * 1024), p.alloc_len);
    try std.testing.expectEqual(@as(usize, 0), p.src_off);
    try std.testing.expectEqual(p.region_len, p.dst_off);
}

test "gpu plan halves are 64 KiB aligned and do not overlap" {
    for ([_]u32{ 0, 1, 4096, 65535, 65536, 65537, 1 << 20, std.math.maxInt(u32) }) |size| {
        const p = plan(size, true);
        try std.testing.expectEqual(@as(usize, 0), p.region_len % gpu_align);
        try std.testing.expectEqual(@as(usize, 0), p.dst_off % gpu_align);
        try std.testing.expect(p.src_off + p.region_len <= p.dst_off);
        try std.testing.expect(p.dst_off + p.region_len <= p.alloc_len);
        try std.testing.expect(p.region_len >= @as(usize, size));
    }
}
