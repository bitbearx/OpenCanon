#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0-or-later
# 真实 Gutenprint 回归：检查页边距、极小输入及双面补白的作业和页命令。
import os
from pathlib import Path
import subprocess
import sys
import tempfile

START_JOB = b"<ivec:operation>StartJob</ivec:operation>"
END_JOB = b"<ivec:operation>EndJob</ivec:operation>"
PAGE_START = b"\x1b(b\x01\x00\x01\x1b(q\x01\x00\x01"
PAGE_END = b"\x0c\x1b(b\x01\x00\x00"


def render_case(helper, directory, name, paper, width, height, duplex, copies, paths):
    """执行真实编码并检查单一作业包裹数量准确、顺序完整的物理输出面。"""
    output = directory / (name + ".prn")
    args = [str(helper), "--render", "--media", paper,
            "--media-type", "photo" if paper == "5x7" else "plain",
            "--color", "color", "--duplex", duplex, "--copies", str(copies),
            "--width-mm", str(width), "--height-mm", str(height), "--"]
    env = dict(os.environ, TMPDIR=str(directory), LC_ALL="C")
    with output.open("wb") as stream:
        result = subprocess.run(args + [str(path) for path in paths], stdout=stream,
                                stderr=subprocess.PIPE, env=env, timeout=180)
    assert result.returncode == 0, (name, result.returncode, result.stderr.decode(errors="replace"))
    assert result.stderr == b"", (name, result.stderr.decode(errors="replace"))
    data = output.read_bytes()
    expected = (len(paths) + (duplex != "single" and len(paths) % 2)) * copies
    assert data.startswith(b"<?xml"), (name, "missing Canon XML header")
    assert data.count(START_JOB) == 1, (name, "incorrect StartJob count")
    assert data.count(END_JOB) == 1, (name, "incorrect EndJob count")
    assert data.count(PAGE_START) == expected, (name, "incorrect page start count")
    assert data.count(PAGE_END) == expected, (name, "incorrect page end count")
    position = data.index(START_JOB) + len(START_JOB)
    for page in range(expected):
        begin = data.index(PAGE_START, position)
        end = data.index(PAGE_END, begin + len(PAGE_START))
        assert data.find(PAGE_START, begin + len(PAGE_START), end) == -1, (name, page, "nested page")
        position = end + len(PAGE_END)
    assert data.index(END_JOB) > position, (name, "EndJob precedes last page")
    print(f"PASS {name}: {expected} pages, one StartJob/EndJob, {len(data)} bytes", flush=True)


def main():
    """生成固定彩色与单像素输入，不接触打印机或修改调用方文件。"""
    helper = Path(sys.argv[1] if len(sys.argv) > 1 else "./opencanon-render").resolve()
    with tempfile.TemporaryDirectory(prefix="opencanon-native-test-") as temporary:
        directory = Path(temporary)
        image = directory / "color.ppm"
        image.write_bytes(b"P6\n24 16\n255\n" + bytes(
            channel for y in range(16) for x in range(24)
            for channel in (255 if x < 8 else 0, 255 if 8 <= x < 16 else 0, 255 if x >= 16 else 0)))
        tiny = directory / "tiny.ppm"
        tiny.write_bytes(b"P6\n1 1\n255\n\xff\xff\xff")
        render_case(helper, directory, "5x7-edge", "5x7", 127, 177.8, "single", 1, [image])
        render_case(helper, directory, "odd-duplex-collated", "A4", 210, 297, "long", 2, [image] * 3)
        render_case(helper, directory, "one-pixel-duplex", "A4", 210, 297, "long", 1, [tiny, tiny])


if __name__ == "__main__":
    main()
