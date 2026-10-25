"""粗粒度死代码检查：找出「定义了但全仓没有任何引用」的函数。

判定规则：
- 统计每个函数名在全部 .go 文件里的出现次数；
- 只出现 1 次（即仅定义处）= 没有任何调用点；
- 测试文件里引用过的也算被使用（可能只被测试用，需人工判断）；
- main / init / TestXxx / String / Error 之类有框架语义的不报。

这是启发式检查，不是编译器级精确分析（Go 没有内建死代码检测），
用途是给出需要人工复核的候选清单，不是直接删。
"""

import re
import sys
from pathlib import Path

FUNC_RE = re.compile(r"^func (?:\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)\s*\(")
SKIP = {"main", "init", "String", "Error", "MarshalJSON", "UnmarshalJSON"}


def main(root: str) -> None:
    files = [p for p in Path(root).rglob("*.go") if "/.git/" not in p.as_posix()]
    sources = {p: p.read_text(encoding="utf-8", errors="replace") for p in files}
    all_text = "\n".join(sources.values())

    defs: dict[str, list[str]] = {}
    for p, txt in sources.items():
        for line in txt.splitlines():
            m = FUNC_RE.match(line)
            if m:
                defs.setdefault(m.group(1), []).append(str(p))

    suspects = []
    for name, where in sorted(defs.items()):
        if name in SKIP or name.startswith("Test") or name.startswith("Benchmark"):
            continue
        # 出现次数：定义 1 次 + 每次调用至少 1 次
        hits = len(re.findall(r"\b" + re.escape(name) + r"\b", all_text))
        if hits <= 1:
            suspects.append((name, where[0], hits))

    if not suspects:
        print("没有发现「零引用」的函数")
        return

    print(f"零引用候选 {len(suspects)} 个（需人工确认是否真的无用）：")
    for name, where, hits in suspects:
        print(f"  {name:34s} {where}  (出现 {hits} 次)")


if __name__ == "__main__":
    main(sys.argv[1])
