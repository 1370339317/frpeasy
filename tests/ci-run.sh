#!/bin/bash
# 跑一条命令；失败时把关键日志行打成 GitHub Actions 注解。
#
# 为什么需要这个：Actions 的**日志下载**接口要求仓库管理员权限
# （`/actions/jobs/{id}/logs` 会返回 403 "Must have admin rights"），
# 而**注解**（check-run annotations）公开可读 —— 于是失败时能直接用 API
# 拿到原因，不必登录网页翻日志。这也让失败摘要出现在 PR 的红色标记里。
#
# 用法：tests/ci-run.sh go test ./... -count=1
set -o pipefail

log=$(mktemp)
"$@" 2>&1 | tee "$log"
rc=$?
if [ "$rc" -eq 0 ]; then
  rm -f "$log"
  exit 0
fi

echo "::error::「$*」失败（exit $rc），下面是关键行"

# 先挑信号最强的行：测试失败、panic、编译错误。注解有条数上限，
# 直接 tail 一大段会挤掉真正有用的那几行。
picked=$(grep -aE '(--- FAIL|^\s+--- FAIL|^FAIL|panic:|_test\.go:[0-9]+:|cannot |undefined:)' "$log" | head -8)
if [ -z "$picked" ]; then
  # 没有典型的失败标记（比如 npm / make 报错）：退回最后几行
  picked=$(tail -8 "$log")
fi

printf '%s\n' "$picked" | while IFS= read -r line; do
  # 去掉 CR 与行首空白：注解里多余空白会让摘要难读
  line=$(printf '%s' "$line" | tr -d '\r' | sed 's/^[[:space:]]*//')
  [ -n "$line" ] && echo "::error::$line"
done
exit "$rc"
