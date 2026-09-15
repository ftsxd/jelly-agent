package codeproject

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
)

// boundedBuffer keeps the first max bytes a command writes and silently drops
// the rest. It always reports a full write so a chatty git never sees a short
// write and gives up early.
type boundedBuffer struct {
	b   bytes.Buffer
	max int
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.max - w.b.Len(); room > 0 {
		if n > room {
			p = p[:room]
		}
		w.b.Write(p)
	}
	return n, nil // the caller wrote all of it; we simply kept less
}

// redact removes the credential from text that is about to be persisted into
// projects.json and shown in the console. Git does not normally echo the
// Authorization header, but "does not normally" is not a property to rely on
// for the one string that must never be written down.
func redact(text, token, auth string) string {
	for _, secret := range []string{auth, token} {
		if len(secret) >= 8 {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}

// excerpt trims git's output to the last few meaningful lines — the fatal one
// is at the end, and the progress noise before it helps nobody.
func excerpt(text string) string {
	var keep []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Cloning into") || strings.HasPrefix(line, "remote: Enumerating") {
			continue
		}
		keep = append(keep, line)
	}
	if len(keep) > 3 {
		keep = keep[len(keep)-3:]
	}
	out := strings.Join(keep, "；")
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	return out
}

// diagnose turns a failed git run into something an operator can act on. The
// point is to separate "your token is wrong" from "your URL is wrong" from
// "there is no such branch" — all three used to arrive as the same sentence.
func diagnose(stderr, token, auth string, p Project, runErr error) string {
	var missing *exec.Error
	if errors.As(runErr, &missing) {
		return "服务端没有安装 git，或它不在 PATH 中"
	}
	s := redact(stderr, token, auth)
	low := strings.ToLower(s)
	detail := excerpt(s)
	with := func(msg string) string {
		if detail == "" {
			return msg
		}
		return msg + "（git: " + detail + "）"
	}

	switch {
	case strings.Contains(low, "could not read username"), strings.Contains(low, "authentication failed"),
		strings.Contains(low, "401"), strings.Contains(low, "invalid username or password"):
		if token == "" {
			return with("仓库需要认证，但这个项目没有配置凭据：请在项目里填写访问令牌")
		}
		return with("凭据被拒绝（401）：请确认访问令牌未过期，以及「Git 用户名」填的是该令牌要求的用户名——多数平台不是你的登录名")
	case strings.Contains(low, "403"), strings.Contains(low, "forbidden"):
		return with("凭据有效但无权访问这个仓库（403）：请确认令牌具备该仓库的只读权限")
	case strings.Contains(low, "not found"), strings.Contains(low, "404"):
		if strings.Contains(low, "remote branch") {
			return with("远端没有分支 " + p.Branch + "：请确认分支名")
		}
		return with("找不到仓库（404）：请确认仓库地址，或令牌是否有权看到它")
	case strings.Contains(low, "couldn't find remote ref"):
		return with("远端没有分支 " + p.Branch + "：请确认分支名")
	case strings.Contains(low, "could not resolve host"), strings.Contains(low, "name or service not known"):
		return with("无法解析仓库域名：请检查服务端的 DNS 和网络出口")
	case strings.Contains(low, "connection refused"), strings.Contains(low, "failed to connect"),
		strings.Contains(low, "timed out"), strings.Contains(low, "connection reset"):
		return with("连不上仓库服务器：请检查服务端网络、代理和防火墙")
	case strings.Contains(low, "ssl"), strings.Contains(low, "certificate"):
		return with("TLS 证书校验失败：企业自签 CA 需要装进服务端的系统信任库")
	case strings.Contains(low, "redirect"):
		return with("仓库地址发生了重定向：请填写重定向后的最终地址")
	}
	return with("拉取失败")
}
