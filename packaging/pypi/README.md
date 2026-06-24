# opencloudcosts PyPI Packaging

This directory contains tooling to publish `opencloudcosts` to PyPI as
platform-specific binary wheels — the same approach used by projects like
[ruff](https://github.com/astral-sh/ruff) and
[uv](https://github.com/astral-sh/uv).

## How it works

Each wheel is a zip file containing:

- The `opencloudcosts` Go binary (pre-compiled, CGO_ENABLED=0) under
  `opencloudcosts/bin/`
- A thin `opencloudcosts/_binary.py` shim that locates the binary at runtime
  and exec-replaces the Python process with it
- A `console_scripts` entry point wired to `opencloudcosts._binary:main`, so
  `pip install opencloudcosts` makes `opencloudcosts` available on `PATH`

There is no Python code to compile and no C extension — the Go binary is the
entire implementation. Each wheel is tagged `py3-none-<platform>` (no CPython
ABI dependency), mirroring the ruff/uv convention.

## Building wheels

1. Run `goreleaser release --clean` (or `goreleaser release --snapshot --clean`
   for a local build) from the repo root. This produces `dist/artifacts.json`
   and the platform archives.

2. From the repo root, run:

   ```
   python3 packaging/pypi/build_wheels.py
   ```

   The script reads `dist/artifacts.json`, extracts each archive, and writes
   one `.whl` file per platform to `dist/wheels/`.

3. Upload to PyPI:

   ```
   pip install twine
   twine upload dist/wheels/*.whl
   ```

   For a test upload first: `twine upload --repository testpypi dist/wheels/*.whl`

## Platform coverage

| GoReleaser target | Wheel platform tag                                     |
|-------------------|--------------------------------------------------------|
| linux/amd64       | manylinux_2_17_x86_64.manylinux2014_x86_64            |
| linux/arm64       | manylinux_2_17_aarch64.manylinux2014_aarch64           |
| darwin/amd64      | macosx_10_9_x86_64                                     |
| darwin/arm64      | macosx_11_0_arm64                                      |
| windows/amd64     | win_amd64                                              |

## Source distribution

`pyproject.toml.template` is a template for a source-dist package. It is not
used by `build_wheels.py`; it exists for reference if a source dist is needed
for PyPI metadata. Copy it to `pyproject.toml`, fill in the version, and build
with `python -m build --sdist` (requires the `build` package).
