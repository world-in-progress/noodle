package util

import "strings"

func ConvertToDNS1035(str string) string {
	newStr := strings.ToLower(str)
	newStr = strings.ReplaceAll(newStr, ".", "-")
	newStr = strings.ReplaceAll(newStr, "_", "-")
	var cleaned []rune
	for _, r := range newStr {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			cleaned = append(cleaned, r)
		}
	}
	newStr = string(cleaned)
	if len(newStr) > 0 && (newStr[0] < 'a' || newStr[0] > 'z') {
		newStr = "a" + newStr
	}
	if len(newStr) > 1 && newStr[len(newStr)-1] == '-' {
		newStr = newStr[:len(newStr)-1] + "0"
	}
	return newStr
}
