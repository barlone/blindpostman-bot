package config

import (
	"bytes"
	"fmt"
	"strings"
)

// iniFile — значения по секциям. Имена секций и ключей приводятся к нижнему регистру.
type iniFile map[string]map[string]string

// parseINI разбирает INI-файл: секции [name], строки key=value, комментарии с ; или #
// в начале строки. Значение можно заключить в двойные кавычки. Комментариев в конце строки
// нет: иначе пострадали бы значения с ; и #, например секреты.
// В ошибки попадает только номер строки, потому что сама строка может содержать секрет.
func parseINI(data []byte) (iniFile, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))

	ini := iniFile{}
	section := ""
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.Trim(line, " \t\r")
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			if line[len(line)-1] != ']' {
				return nil, fmt.Errorf("line %d: unterminated section header", i+1)
			}
			section = strings.ToLower(strings.Trim(line[1:len(line)-1], " \t"))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key=value", i+1)
		}
		if section == "" {
			return nil, fmt.Errorf("line %d: key outside of a section", i+1)
		}
		if ini[section] == nil {
			ini[section] = map[string]string{}
		}
		ini[section][strings.ToLower(strings.Trim(key, " \t"))] = unquote(strings.Trim(value, " \t"))
	}
	return ini, nil
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
