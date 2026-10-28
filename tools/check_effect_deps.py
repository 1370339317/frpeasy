#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
扫描 web/src 下的 useEffect，找出"依赖里含 store 数组/对象"的写法。

为什么需要这个检查
------------------
React 的依赖比较是**引用相等**（Object.is），不做深比较。而 store 里的
`agents` / `tunnels` / `settings` / `versions` 每次刷新都会产生**新引用**
（`r.agents ?? []` 就是新数组字面量），于是任何把这类值放进依赖数组的
useEffect 都会在每次轮询后重跑一次。

如果那个 effect 里写的是表单 state（setName / setLocal / setSelVer…），
结果就是"用户正在输入的内容被后台刷新覆盖"。这类 bug 在代码里看不出错——
语法对、类型对、跑起来也不报错，只有人坐在那儿打字才会发现。

所以用静态检查挡住它，比靠记忆和 review 可靠。规则很简单：
**写表单 state 的 effect，依赖里不许出现 store 的数组/对象。**

已知豁免：函数体里有 seededRef / hydrated / touched 之类的守卫，
说明作者已经显式处理了"只在初始化时取值"，此时把 store 值放进依赖是安全的。

用法：
    python tools/check_effect_deps.py            # 扫描并报告
    python tools/check_effect_deps.py --strict   # 有命中就非 0 退出（可挂 CI）

退出码：0 = 无问题；1 = 有命中（仅 --strict 下）。
"""

import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(ROOT, "web", "src")

# store 里会"每轮都变"的字段。命中这些才值得报，普通局部 state 不报。
STORE_FIELDS = {
    "agents", "agentMap", "tunnels", "settings", "versions", "frps",
    "net", "me", "install", "uninstall", "accessHost", "accessSource",
    "installBase", "sourceMode", "lastRank", "panelPort", "toasts",
}

# 函数体里出现这些名字，说明作者做了"只初始化一次"的守卫，视为已处理。
GUARD_HINTS = ("seededRef", "hydrated", "verTouched", "srcHydrated", "dirtyRef")
# 显式豁免注释
EXEMPT_MARK = "effect-deps-ok"


def find_use_effects(text):
    """返回 [(起始行号, 函数体, 依赖数组原文)]。用括号配平，不靠正则猜。"""
    out = []
    for m in re.finditer(r"\buseEffect\s*\(", text):
        i = m.end() - 1  # 指向 '('
        depth = 0
        j = i
        in_str = None
        while j < len(text):
            ch = text[j]
            if in_str:
                if ch == "\\":
                    j += 2
                    continue
                if ch == in_str:
                    in_str = None
            elif ch in "'\"`":
                in_str = ch
            elif ch == "(":
                depth += 1
            elif ch == ")":
                depth -= 1
                if depth == 0:
                    break
            j += 1
        call = text[i + 1:j]
        # 依赖数组是调用参数里最后一个 [...]，取最后一个 '['
        k = call.rfind("[")
        if k < 0:
            continue
        deps = call[k:]
        body = call[:k]
        line = text[: m.start()].count("\n") + 1
        out.append((line, body, deps))
    return out


def scan_file(path):
    with open(path, encoding="utf-8") as f:
        text = f.read()
    hits = []
    for line, body, deps in find_use_effects(text):
        names = set(re.findall(r"[A-Za-z_$][\w$]*", deps))
        bad = sorted(names & STORE_FIELDS)
        if not bad:
            continue
        if any(h in body for h in GUARD_HINTS):
            continue
        if EXEMPT_MARK in body or EXEMPT_MARK in deps:
            continue
        # 只看真正写表单的情况：函数体里有 setXxx(
        if not re.search(r"\bset[A-Z]\w*\s*\(", body):
            continue
        hits.append((line, bad, deps.strip()[:80]))
    return hits


def main():
    strict = "--strict" in sys.argv
    total = 0
    files = 0
    for dirpath, _, filenames in os.walk(SRC):
        for fn in filenames:
            if not fn.endswith((".tsx", ".ts")):
                continue
            path = os.path.join(dirpath, fn)
            files += 1
            for line, bad, deps in scan_file(path):
                total += 1
                rel = os.path.relpath(path, ROOT)
                print(f"{rel}:{line}  依赖含 store 字段 {bad}")
                print("    } " + deps)
    print()
    print(f"扫描 {files} 个文件，命中 {total} 处")
    if total:
        print("提示：把要用的值放进 ref、依赖只留 [open]，或加 'effect-deps-ok' 注释说明理由。")
    if strict and total:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
