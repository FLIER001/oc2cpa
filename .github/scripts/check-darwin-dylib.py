#!/usr/bin/env python3
"""Check the macOS deployment target and ABI exports of a built .dylib.

CI builds the darwin plugin on a macOS runner, where clang defaults the
deployment target to the *runner's* OS version. That silently produced dylibs
declaring `minos 15.0` — a plugin that tells every Mac on macOS 11-14 it was
built for a newer system, which is how issue #4 (macOS 12.7.2) was reported.
The target is now pinned with MACOSX_DEPLOYMENT_TARGET; this script keeps it
pinned, because the mistake is invisible in the build log.

It also verifies the C ABI entry point survived packaging: the host finds the
plugin with dlsym("cliproxy_plugin_init"), so a stripped or renamed export means
the plugin loads as "missing cliproxy_plugin_init".

Usage:
    python3 scripts/check-darwin-dylib.py <path/to/workbuddy.dylib>
    python3 scripts/check-darwin-dylib.py <path> --max-minos 11.0
"""

from __future__ import annotations

import argparse
import struct
import sys

MH_MAGIC_64 = 0xFEEDFACF
LC_BUILD_VERSION = 0x32
LC_VERSION_MIN_MACOSX = 0x24
LC_SYMTAB = 0x2
LC_ID_DYLIB = 0xD

PLATFORM_MACOS = 1

N_EXT = 0x01
N_TYPE = 0x0E
N_UNDF = 0x00
N_SECT = 0x0E

# Exported by the plugin and looked up by the host.
REQUIRED_EXPORTS = ("_cliproxy_plugin_init",)


def decode_version(packed: int) -> tuple[int, int, int]:
    return (packed >> 16, (packed >> 8) & 0xFF, packed & 0xFF)


def format_version(version: tuple[int, int, int]) -> str:
    return "%d.%d.%d" % version


def parse_load_commands(blob: bytes) -> dict:
    if len(blob) < 32:
        raise ValueError("file is too small to be a Mach-O image")
    magic, = struct.unpack_from("<I", blob, 0)
    if magic != MH_MAGIC_64:
        raise ValueError("not a 64-bit Mach-O image (magic %#x)" % magic)

    ncmds, = struct.unpack_from("<I", blob, 16)
    flags, = struct.unpack_from("<I", blob, 24)
    info: dict = {"flags": flags, "build_version": None, "version_min": None, "symbols": []}

    offset = 32
    for _ in range(ncmds):
        cmd, size = struct.unpack_from("<II", blob, offset)
        if size < 8:
            raise ValueError("load command %#x has an invalid size %d" % (cmd, size))
        if cmd == LC_BUILD_VERSION:
            platform, minos, sdk = struct.unpack_from("<III", blob, offset + 8)
            info["build_version"] = {
                "platform": platform,
                "minos": decode_version(minos),
                "sdk": decode_version(sdk),
            }
        elif cmd == LC_VERSION_MIN_MACOSX:
            version, sdk = struct.unpack_from("<II", blob, offset + 8)
            info["version_min"] = {
                "platform": PLATFORM_MACOS,
                "minos": decode_version(version),
                "sdk": decode_version(sdk),
            }
        elif cmd == LC_SYMTAB:
            symoff, nsyms, stroff, _ = struct.unpack_from("<IIII", blob, offset + 8)
            info["symbols"] = read_symbols(blob, symoff, nsyms, stroff)
        elif cmd == LC_ID_DYLIB:
            name_offset, = struct.unpack_from("<I", blob, offset + 8)
            raw = blob[offset + name_offset:offset + size].split(b"\0")[0]
            info["install_name"] = raw.decode("utf-8", "replace")
        offset += size
    return info


def read_symbols(blob: bytes, symoff: int, nsyms: int, stroff: int) -> list[tuple[str, int]]:
    symbols = []
    for index in range(nsyms):
        entry = symoff + index * 16
        n_strx, n_type, _n_sect, _n_desc, _n_value = struct.unpack_from("<IBBHQ", blob, entry)
        end = blob.index(b"\0", stroff + n_strx)
        name = blob[stroff + n_strx:end].decode("utf-8", "replace")
        symbols.append((name, n_type))
    return symbols


def exported_symbols(info: dict) -> set[str]:
    defined = set()
    for name, n_type in info["symbols"]:
        if not n_type & N_EXT:
            continue
        if n_type & N_TYPE == N_SECT:
            defined.add(name)
    return defined


def deployment_target(info: dict) -> tuple[tuple[int, int, int], tuple[int, int, int]] | None:
    for key in ("build_version", "version_min"):
        entry = info.get(key)
        if entry and entry["platform"] == PLATFORM_MACOS:
            return entry["minos"], entry["sdk"]
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("library", help="path to the built .dylib")
    parser.add_argument(
        "--max-minos",
        default="11.0",
        help="highest acceptable deployment target (default: 11.0, the oldest macOS Go 1.26 supports)",
    )
    args = parser.parse_args()

    with open(args.library, "rb") as handle:
        blob = handle.read()

    try:
        info = parse_load_commands(blob)
    except ValueError as error:
        print("FAIL  %s: %s" % (args.library, error))
        return 1

    max_minos = tuple(int(part) for part in (args.max_minos.split(".") + ["0", "0"])[:3])
    failures = []

    target = deployment_target(info)
    if target is None:
        failures.append("no LC_BUILD_VERSION/LC_VERSION_MIN_MACOSX with platform=macOS: the dylib does not declare a macOS deployment target")
        print("      minos: (none)")
    else:
        minos, sdk = target
        print("      minos: %s (sdk %s)" % (format_version(minos), format_version(sdk)))
        if minos > max_minos:
            failures.append(
                "deployment target %s is newer than %s: Macs on older macOS are told the plugin was built for a newer system"
                % (format_version(minos), format_version(max_minos))
            )

    exports = exported_symbols(info)
    missing = [symbol for symbol in REQUIRED_EXPORTS if symbol not in exports]
    if missing:
        failures.append(
            "missing exported symbol(s) %s: the host loads the plugin with dlsym(\"cliproxy_plugin_init\")"
            % ", ".join(missing)
        )
    print("      exports: %d symbols (required: %s)" % (len(exports), ", ".join(REQUIRED_EXPORTS)))

    flags = info["flags"]
    flat = bool(flags & 0x100)  # MH_FORCE_FLAT
    print("      namespace: %s" % ("FLAT" if flat else "two-level"))

    if failures:
        for failure in failures:
            print("FAIL  %s" % failure)
        return 1
    print("ok    darwin dylib check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
