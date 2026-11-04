package printer

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

// interval 表示包含两端的页码区间。
type interval struct{ first, last int }

// NormalizeRanges 校验并合并页码区间，返回规范值与页数。
func NormalizeRanges(input string, max int) (string, int, error) {
	if max < 1 {
		return "", 0, errors.New("invalid page count")
	}
	input = strings.TrimSpace(strings.ReplaceAll(input, "，", ","))
	if input == "" {
		return "1-" + strconv.Itoa(max), max, nil
	}
	var spans []interval
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return "", 0, errors.New("empty range")
		}
		pieces := strings.Split(part, "-")
		if len(pieces) > 2 {
			return "", 0, errors.New("invalid range")
		}
		parse := func(s string) (int, error) {
			s = strings.TrimSpace(s)
			if s == "" {
				return 0, errors.New("empty page")
			}
			for _, c := range s {
				if c < '0' || c > '9' {
					return 0, errors.New("invalid page")
				}
			}
			n, e := strconv.Atoi(s)
			if e != nil || n < 1 || n > max {
				return 0, errors.New("page out of range")
			}
			return n, nil
		}
		first, err := parse(pieces[0])
		if err != nil {
			return "", 0, err
		}
		last := first
		if len(pieces) == 2 {
			last, err = parse(pieces[1])
			if err != nil || last < first {
				return "", 0, errors.New("invalid range")
			}
		}
		spans = append(spans, interval{first, last})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].first < spans[j].first })
	merged := spans[:0]
	for _, s := range spans {
		if len(merged) > 0 && s.first <= merged[len(merged)-1].last+1 {
			if s.last > merged[len(merged)-1].last {
				merged[len(merged)-1].last = s.last
			}
		} else {
			merged = append(merged, s)
		}
	}
	var parts []string
	count := 0
	for _, s := range merged {
		count += s.last - s.first + 1
		if s.first == s.last {
			parts = append(parts, strconv.Itoa(s.first))
		} else {
			parts = append(parts, strconv.Itoa(s.first)+"-"+strconv.Itoa(s.last))
		}
	}
	return strings.Join(parts, ","), count, nil
}

// PageNumbers 将 NormalizeRanges 的规范结果展开为逐页渲染顺序。
func PageNumbers(normalized string) []int {
	var pages []int
	for _, part := range strings.Split(normalized, ",") {
		bounds := strings.SplitN(part, "-", 2)
		first, err := strconv.Atoi(bounds[0])
		if err != nil || first < 1 {
			return nil
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(bounds[1])
			if err != nil || last < first {
				return nil
			}
		}
		for page := first; page <= last; page++ {
			pages = append(pages, page)
		}
	}
	return pages
}
