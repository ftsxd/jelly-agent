package execution

import (
	"fmt"
	"strings"
)

type Command struct {
	Segments  [][]string
	Operators []string
}

// Parse supports a deliberately small shell grammar: literal arguments, quotes,
// pipes, &&, ||, and ;. Expansions, redirections, assignments, background jobs,
// globbing, control flow and subshells fail closed. Raw input never reaches sh.
func Parse(input string) (Command, error) {
	var out Command
	if len(input) > 32<<10 {
		return out, fmt.Errorf("命令不能超过 32 KiB")
	}
	var word strings.Builder
	var argv []string
	var quote byte
	started := false
	flushWord := func() {
		if started {
			argv = append(argv, word.String())
			word.Reset()
			started = false
		}
	}
	flushSegment := func() error {
		flushWord()
		if len(argv) == 0 {
			return fmt.Errorf("命令分段不能为空")
		}
		if !identifier.MatchString(argv[0]) {
			return fmt.Errorf("命令名必须是普通可执行文件名；不接受路径、赋值或包装语法")
		}
		out.Segments = append(out.Segments, argv)
		argv = nil
		return nil
	}
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if ch == 0 || ch == '\r' || ch == '\n' {
			return Command{}, fmt.Errorf("不支持换行、heredoc 或空字节")
		}
		if quote == '\'' {
			if ch == '\'' {
				quote = 0
			} else {
				word.WriteByte(ch)
			}
			continue
		}
		if ch == '\\' {
			if i+1 >= len(input) {
				return Command{}, fmt.Errorf("不完整的转义")
			}
			i++
			if input[i] == '\n' || input[i] == '\r' || input[i] == 0 {
				return Command{}, fmt.Errorf("不支持续行或空字节")
			}
			// Inside double quotes, only these escapes are POSIX escapes. Reject
			// the others rather than changing the meaning of a CLI query.
			if quote == '"' && !strings.ContainsRune("$`\"\\", rune(input[i])) {
				return Command{}, fmt.Errorf("双引号内不支持此转义")
			}
			word.WriteByte(input[i])
			started = true
			continue
		}
		if ch == '$' || ch == '`' {
			return Command{}, fmt.Errorf("不支持变量展开或命令替换；凭据由运行时注入")
		}
		if quote == '"' {
			if ch == '"' {
				quote = 0
			} else {
				word.WriteByte(ch)
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			started = true
			continue
		}
		if ch == ' ' || ch == '\t' {
			flushWord()
			continue
		}
		if ch == '|' || ch == '&' || ch == ';' {
			op := string(ch)
			if i+1 < len(input) && input[i+1] == ch && ch != ';' {
				op += string(ch)
				i++
			}
			if op == "&" {
				return Command{}, fmt.Errorf("不支持后台执行")
			}
			if err := flushSegment(); err != nil {
				return Command{}, err
			}
			out.Operators = append(out.Operators, op)
			continue
		}
		if strings.ContainsRune("<>()*?[{}~#", rune(ch)) {
			return Command{}, fmt.Errorf("不支持重定向、子 shell、通配符或控制语法，请将查询参数用单引号括起")
		}
		word.WriteByte(ch)
		started = true
	}
	if quote != 0 {
		return Command{}, fmt.Errorf("引号未闭合")
	}
	if err := flushSegment(); err != nil {
		return Command{}, err
	}
	if len(out.Segments) > 16 {
		return Command{}, fmt.Errorf("一次最多执行 16 个命令分段")
	}
	return out, nil
}

// Script reconstructs only the accepted argv and operators. A literal $(),
// newline or quote inside a single-quoted CLI argument stays data.
func (c Command) Script() string {
	var b strings.Builder
	for i, argv := range c.Segments {
		if i > 0 {
			b.WriteString(" " + c.Operators[i-1] + " ")
		}
		for j, arg := range argv {
			if j > 0 {
				b.WriteByte(' ')
			}
			b.WriteString("'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'")
		}
	}
	return b.String()
}
