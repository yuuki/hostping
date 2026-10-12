const std = @import("std");

var threaded: std.Io.Threaded = .init_single_threaded;

pub fn io() std.Io {
    return threaded.io();
}

pub fn readAllAbs(path: []const u8, buf: []u8) ![]const u8 {
    const i = io();
    var file = try std.Io.Dir.openFileAbsolute(i, path, .{});
    defer file.close(i);
    const n = try file.readPositionalAll(i, buf, 0);
    return buf[0..n];
}

pub fn nowNs() u64 {
    const ns = std.Io.Clock.now(.real, io()).toNanoseconds();
    return if (ns <= 0) 0 else @intCast(ns);
}
