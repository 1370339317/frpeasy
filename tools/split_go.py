"""按函数归属切分 Go 源文件（通用版）。

用法：
    python split_go.py <源文件> <输出目录> <组名>=<函数1,函数2> [<组名>=...]

每个函数连同它前面的文档注释整块搬走。文件头（package/import/type/const）
永远归到 "-" 分组，由调用方决定放在哪个文件里。

为什么要脚本而不是手数行号：这类重构最常见的错误就是边界差一行，
把下一个函数的注释留在上一个文件、或反过来丢掉一行语句。
"""

import re
import sys
from pathlib import Path

FUNC_RE = re.compile(r"^func (?:\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)\s*\(")


def main(argv: list[str]) -> None:
    src, outdir = argv[0], argv[1]
    groups: dict[str, list[str]] = {}
    for spec in argv[2:]:
        name, _, funcs = spec.partition("=")
        groups[name] = [f for f in funcs.split(",") if f]

    lines = Path(src).read_text(encoding="utf-8").splitlines(keepends=True)
    starts = [i for i, ln in enumerate(lines) if FUNC_RE.match(ln)]
    if not starts:
        sys.exit("没找到任何 func")

    # 先算出每个 func 的真实起点（向前吞掉属于它的注释与空行），
    # 再令每块结尾等于下一块起点 —— 否则注释块会同时落进相邻两个文件。
    starts_b: list[int] = []
    for s in starts:
        b = s
        while b > 0 and (lines[b - 1].lstrip().startswith("//") or lines[b - 1].strip() == ""):
            b -= 1
        starts_b.append(b)

    chunks: dict[str, list[str]] = {g: [] for g in groups}
    header = "".join(lines[: starts_b[0]]).rstrip() + "\n"

    owner: dict[str, str] = {}
    for grp, names in groups.items():
        for n in names:
            if n in owner:
                sys.exit(f"函数 {n} 被分到多个组")
            owner[n] = grp

    unknown: list[str] = []
    for idx, s in enumerate(starts):
        name = FUNC_RE.match(lines[s]).group(1)
        grp = owner.get(name)
        if grp is None:
            unknown.append(name)
            continue
        e = starts_b[idx + 1] if idx + 1 < len(starts_b) else len(lines)
        body = "".join(lines[starts_b[idx] : e])
        chunks[grp].append(body.rstrip() + "\n")

    if unknown:
        sys.exit("有函数没分配组: " + ", ".join(unknown))

    out = Path(outdir)
    out.mkdir(parents=True, exist_ok=True)
    for grp, parts in chunks.items():
        text = "\n".join(parts)
        (out / f"{grp}.body").write_text(text, encoding="utf-8")
        print(f"{grp:14s} {len(text.splitlines()):4d} 行  {len(parts)} 个函数")
    (out / "_header.txt").write_text(header, encoding="utf-8")
    print(f"{'_header':14s} {len(header.splitlines()):4d} 行")


if __name__ == "__main__":
    main(sys.argv[1:])
