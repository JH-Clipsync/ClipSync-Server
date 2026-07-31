#!/bin/bash
# 模拟手机端发一条 sms_code 消息给 Mac 端测试 Toast
MSG='{"id":"toast-'$(date +%s)'","type":"sms_code","from":"phone-sim","to":"pc-*","ts":0,"payload":{"text":"【+8618515321915】测试Toast弹窗，您的验证码是 314159，5分钟内有效","mime":"text/plain","preview":"来自 +8618515321915 的短信"}}'
echo "$MSG" | wscat -c "ws://localhost:8080/ws?token=test123&device=phone-sim-$(date +%s)&role=phone" -x - -w 1 2>&1
