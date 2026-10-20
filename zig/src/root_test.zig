const std = @import("std");

test {
    _ = @import("abi.zig");
    _ = @import("sim.zig");
    _ = @import("gdr.zig");
    _ = @import("loopbuf.zig");
    _ = @import("memwarm.zig");
    _ = @import("errclass.zig");
    _ = @import("detail.zig");
    _ = @import("udtag.zig");
    _ = @import("portaddr.zig");
    _ = @import("verbs_types.zig");
}

test "the Darwin verbs stub compiles on every platform" {
    // A Linux build never reaches verbs_stub.zig through verbs.zig, which
    // picks verbs_linux.zig. Referencing every declaration analyses the
    // stub's bodies here; verbs.zig checks its signatures at compile time.
    std.testing.refAllDecls(@import("verbs_stub.zig"));
}
