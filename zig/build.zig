const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    // An explicit x86_64 target (cross-compiling from the aarch64 Apple
    // Silicon container, or pinning the glibc version as the release build
    // does) does not search the host's system paths, so point it at the
    // Debian multiarch paths for libibverbs.
    const cross_x86_64_linux = target.result.os.tag == .linux and
        target.result.cpu.arch == .x86_64 and !target.query.isNative();

    // Zig 0.17 removed @cImport, so C headers are translated here and
    // imported by module name. The built-in TranslateC step is deprecated in
    // favour of the external translate-c package, but it needs no package
    // fetch, which keeps offline builds working.
    //
    // The public C ABI header, compared against the Zig extern structs at
    // compile time in src/abi.zig. Target-independent: it is our own header.
    const hostping_h = b.addTranslateC(.{
        .root_source_file = b.path("../include/hostping.h"),
        .target = target,
        .optimize = optimize,
    }).createModule();

    const probe = b.createModule(.{
        .root_source_file = b.path("src/capi.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });
    probe.addImport("hostping_h", hostping_h);
    if (target.result.os.tag == .linux) {
        const verbs_c = b.addTranslateC(.{
            .root_source_file = b.path("src/verbs_c.h"),
            .target = target,
            .optimize = optimize,
        });
        if (cross_x86_64_linux) {
            verbs_c.addSystemIncludePath(.{ .cwd_relative = "/usr/include" });
            probe.addLibraryPath(.{ .cwd_relative = "/usr/lib/x86_64-linux-gnu" });
        }
        probe.addImport("verbs_c", verbs_c.createModule());
        probe.linkSystemLibrary("ibverbs", .{});
    }

    const lib = b.addLibrary(.{
        .name = "hostping_probe",
        .linkage = .dynamic,
        .root_module = probe,
    });
    lib.bundle_compiler_rt = true;
    b.installArtifact(lib);

    const test_mod = b.createModule(.{
        .root_source_file = b.path("src/root_test.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });
    test_mod.addImport("hostping_h", hostping_h);
    const tests = b.addTest(.{ .root_module = test_mod });
    const run_tests = b.addRunArtifact(tests);
    const test_step = b.step("test", "Run Zig unit tests");
    test_step.dependOn(&run_tests.step);
}
