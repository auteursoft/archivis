package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestYearRange(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := func(y int, m time.Month, d, h int) int64 { return time.Date(y, m, d, h, 0, 0, 0, time.UTC).Unix() }
	shots := map[string]int64{
		"1968": at(1968, 6, 1, 12), "1969end": at(1969, 12, 31, 23), "1970start": at(1970, 1, 1, 1),
		"2019end": at(2019, 12, 31, 23), "2020start": at(2020, 1, 1, 0), "2021": at(2021, 7, 4, 9),
		"2024": at(2024, 2, 29, 8), "undated": 0,
	}
	var rs []*Result
	for name, t0 := range shots {
		rs = append(rs, &Result{Photo: Photo{Path: "/" + name, Size: 1, MTime: 1, Fingerprint: name, TakenAt: t0, Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	names := func(f Filter) string {
		f.Sort, f.Limit = "path", 100
		ps, _, err := st.Browse(f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range ps {
			out = append(out, strings.TrimPrefix(p.Path, "/"))
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		f    Filter
		want string
	}{
		{Filter{Year: 2020}, "2020start"},
		{Filter{Year: 2019}, "2019end"},
		{Filter{YearFrom: 2019, YearTo: 2021}, "2019end,2020start,2021"},
		{Filter{YearFrom: 2021, YearTo: 2019}, "2019end,2020start,2021"}, // reversed (the CLI passes it as given)
		{Filter{YearFrom: 2021}, "2021,2024"},
		{Filter{YearTo: 1969}, "1968,1969end"}, // before 1970: negative times, undated excluded
		{Filter{Year: 1970}, "1970start"},
		{Filter{YearFrom: 1969, YearTo: 1970}, "1969end,1970start"},
		{Filter{Year: 2022}, ""},
	} {
		if got := names(tc.f); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.f, got, tc.want)
		}
	}

	ys, err := st.Years()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ys) != "[1968 1969 1970 2019 2020 2021 2024]" {
		t.Fatalf("Years() = %v", ys)
	}

	// the year range is served by the taken_at index
	where, args := Filter{YearFrom: 2019, YearTo: 2021}.where()
	rows, err := st.DB.Query("EXPLAIN QUERY PLAN SELECT id FROM photos"+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		plan = append(plan, detail)
	}
	if p := strings.Join(plan, "; "); !strings.Contains(p, "photos_taken") && !strings.Contains(p, "photos_burst") {
		t.Fatalf("year range does not use a taken_at index: %s", p)
	}
}
