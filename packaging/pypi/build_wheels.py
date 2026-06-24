#!/usr/bin/env python3
"""Build platform-specific PyPI wheels from GoReleaser artifacts.

Usage (from repo root after goreleaser has run):
    python3 packaging/pypi/build_wheels.py

Reads dist/artifacts.json and wraps each Archive into a .whl file under
dist/wheels/. Requires Python 3.8+, stdlib only.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import stat
import sys
import tarfile
import tempfile
import zipfile
from pathlib import Path

# ---------------------------------------------------------------------------
# Platform tag mapping: (goos, goarch) -> wheel platform tag
# ---------------------------------------------------------------------------
PLATFORM_TAGS: dict[tuple[str, str], str] = {
    ("linux", "amd64"): "manylinux_2_17_x86_64.manylinux2014_x86_64",
    ("linux", "arm64"): "manylinux_2_17_aarch64.manylinux2014_aarch64",
    ("darwin", "amd64"): "macosx_10_9_x86_64",
    ("darwin", "arm64"): "macosx_11_0_arm64",
    ("windows", "amd64"): "win_amd64",
}

PACKAGE_NAME = "opencloudcosts"
AUTHOR = "x7even"
SUMMARY = "MCP server for cloud pricing (AWS, GCP, Azure)"
LICENSE = "MIT"
REQUIRES_PYTHON = ">=3.8"

# Wheel Python tag: py3-none-<platform> — no CPython ABI dependency.
PYTHON_TAG = "py3"
ABI_TAG = "none"


def _sha256_urlsafe_b64(data: bytes) -> str:
    """Return urlsafe-base64-encoded SHA-256 of data, with padding stripped."""
    digest = hashlib.sha256(data).digest()
    return base64.urlsafe_b64encode(digest).rstrip(b"=").decode("ascii")


def _record_entry(path: str, data: bytes) -> str:
    """Return a RECORD line for a file: path,sha256=<hash>,<size>"""
    return f"{path},sha256={_sha256_urlsafe_b64(data)},{len(data)}"


def extract_binary_from_archive(archive_path: str, goos: str) -> bytes:
    """Extract the opencloudcosts binary from a tar.gz or zip archive."""
    binary_name = "opencloudcosts.exe" if goos == "windows" else "opencloudcosts"

    if archive_path.endswith(".zip"):
        with zipfile.ZipFile(archive_path) as zf:
            for name in zf.namelist():
                if name.endswith(binary_name) and not name.endswith("/"):
                    return zf.read(name)
        raise FileNotFoundError(
            f"Binary {binary_name!r} not found in {archive_path}"
        )
    else:
        # Assume .tar.gz
        with tarfile.open(archive_path, "r:gz") as tf:
            for member in tf.getmembers():
                if member.name.endswith(binary_name) and member.isfile():
                    f = tf.extractfile(member)
                    if f is None:
                        raise FileNotFoundError(
                            f"Could not read {member.name!r} from {archive_path}"
                        )
                    return f.read()
        raise FileNotFoundError(
            f"Binary {binary_name!r} not found in {archive_path}"
        )


def _init_py_content(version: str) -> bytes:
    return f'__version__ = "{version}"\n'.encode()


def _binary_py_content(goos: str) -> bytes:
    binary_filename = "opencloudcosts.exe" if goos == "windows" else "opencloudcosts"
    content = f'''\
"""Shim that locates the bundled opencloudcosts binary and runs it."""
from __future__ import annotations

import os
import stat
import subprocess
import sys
from pathlib import Path


def _binary_path() -> str:
    pkg_dir = Path(__file__).parent
    name = "{binary_filename}"
    path = pkg_dir / "bin" / name
    # pip does not mark package-data files executable; fix at runtime.
    if os.name != "nt":
        current = path.stat().st_mode
        if not (current & stat.S_IXUSR):
            path.chmod(current | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return str(path)


def main() -> None:
    binary = _binary_path()
    args = [binary] + sys.argv[1:]
    if os.name == "nt":
        # os.execv is unreliable on Windows; use subprocess instead.
        proc = subprocess.run(args)
        sys.exit(proc.returncode)
    else:
        os.execv(binary, args)
'''
    return content.encode()


def _metadata_content(version: str) -> bytes:
    return (
        f"Metadata-Version: 2.1\n"
        f"Name: {PACKAGE_NAME}\n"
        f"Version: {version}\n"
        f"Summary: {SUMMARY}\n"
        f"Author: {AUTHOR}\n"
        f"License: {LICENSE}\n"
        f"Requires-Python: {REQUIRES_PYTHON}\n"
    ).encode()


def _wheel_content(python_tag: str, abi_tag: str, platform_tag: str) -> bytes:
    return (
        f"Wheel-Version: 1.0\n"
        f"Generator: build_wheels.py\n"
        f"Root-Is-Purelib: false\n"
        f"Tag: {python_tag}-{abi_tag}-{platform_tag}\n"
    ).encode()


def _entry_points_content() -> bytes:
    return (
        "[console_scripts]\n"
        f"opencloudcosts = opencloudcosts._binary:main\n"
    ).encode()


def build_wheel(
    version: str,
    goos: str,
    goarch: str,
    platform_tag: str,
    binary_data: bytes,
    output_dir: Path,
) -> Path:
    """Build a single platform wheel and return its path."""
    dist_info = f"{PACKAGE_NAME}-{version}.dist-info"
    binary_filename = "opencloudcosts.exe" if goos == "windows" else "opencloudcosts"
    bin_path = f"opencloudcosts/bin/{binary_filename}"

    wheel_tag = f"{PYTHON_TAG}-{ABI_TAG}-{platform_tag}"
    wheel_name = f"{PACKAGE_NAME}-{version}-{wheel_tag}.whl"
    wheel_path = output_dir / wheel_name

    # Pre-compute all file contents so we can build the RECORD.
    files: dict[str, bytes] = {}

    files["opencloudcosts/__init__.py"] = _init_py_content(version)
    files["opencloudcosts/_binary.py"] = _binary_py_content(goos)
    files[bin_path] = binary_data
    files[f"{dist_info}/METADATA"] = _metadata_content(version)
    files[f"{dist_info}/WHEEL"] = _wheel_content(PYTHON_TAG, ABI_TAG, platform_tag)
    files[f"{dist_info}/entry_points.txt"] = _entry_points_content()

    # Build RECORD (every file except RECORD itself gets a hash entry).
    record_lines = [_record_entry(path, data) for path, data in files.items()]
    record_lines.append(f"{dist_info}/RECORD,,")
    record_data = "\n".join(record_lines).encode()

    output_dir.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(wheel_path, "w", compression=zipfile.ZIP_DEFLATED) as zf:
        for path, data in files.items():
            zi = zipfile.ZipInfo(path)
            zi.compress_type = zipfile.ZIP_DEFLATED
            if path == bin_path:
                # Set executable bit in external_attr so tools that honour it
                # (e.g. unzip) will mark the binary executable.
                zi.external_attr = (stat.S_IFREG | 0o755) << 16
            else:
                zi.external_attr = (stat.S_IFREG | 0o644) << 16
            zf.writestr(zi, data)

        zi_record = zipfile.ZipInfo(f"{dist_info}/RECORD")
        zi_record.compress_type = zipfile.ZIP_DEFLATED
        zi_record.external_attr = (stat.S_IFREG | 0o644) << 16
        zf.writestr(zi_record, record_data)

    print(f"  wrote {wheel_path.name}")
    return wheel_path


def main() -> None:
    repo_root = Path(__file__).resolve().parent.parent.parent
    artifacts_path = repo_root / "dist" / "artifacts.json"
    output_dir = repo_root / "dist" / "wheels"

    if not artifacts_path.exists():
        print(
            f"error: {artifacts_path} not found\n"
            "Run goreleaser first: goreleaser release --clean",
            file=sys.stderr,
        )
        sys.exit(1)

    with artifacts_path.open() as f:
        artifacts = json.load(f)

    # Extract version from any artifact (they all share the same version).
    version = None
    for art in artifacts:
        if art.get("extra", {}).get("ID") == "opencloudcosts":
            version = art.get("extra", {}).get("Tag") or art.get("name", "").split("_")[1]
        if not version:
            version = art.get("extra", {}).get("Tag")
        if version:
            # Strip leading 'v'.
            if version.startswith("v"):
                version = version[1:]
            break

    # Fallback: parse from first archive name like opencloudcosts_1.0.0_linux_amd64.tar.gz
    if not version:
        for art in artifacts:
            if art.get("type") == "Archive":
                name = os.path.basename(art.get("name", ""))
                parts = name.split("_")
                if len(parts) >= 2:
                    version = parts[1]
                    break

    if not version:
        print("error: could not determine version from artifacts.json", file=sys.stderr)
        sys.exit(1)

    print(f"Building wheels for opencloudcosts {version}")

    archives = [a for a in artifacts if a.get("type") == "Archive"]
    if not archives:
        print("error: no Archive entries found in artifacts.json", file=sys.stderr)
        sys.exit(1)

    built = 0
    for art in archives:
        goos = art.get("goos", "")
        goarch = art.get("goarch", "")
        platform_tag = PLATFORM_TAGS.get((goos, goarch))
        if not platform_tag:
            print(f"  skipping unknown platform {goos}/{goarch}")
            continue

        archive_path = art.get("path", "")
        if not os.path.isabs(archive_path):
            archive_path = str(repo_root / archive_path)

        if not os.path.exists(archive_path):
            print(f"  warning: archive not found: {archive_path}", file=sys.stderr)
            continue

        print(f"  processing {goos}/{goarch} ({os.path.basename(archive_path)})")
        try:
            binary_data = extract_binary_from_archive(archive_path, goos)
        except FileNotFoundError as e:
            print(f"  error: {e}", file=sys.stderr)
            continue

        build_wheel(version, goos, goarch, platform_tag, binary_data, output_dir)
        built += 1

    print(f"\n{built} wheel(s) written to {output_dir}/")
    if built == 0:
        sys.exit(1)


if __name__ == "__main__":
    main()
