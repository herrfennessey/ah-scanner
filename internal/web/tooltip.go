package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/herrfennessey/ah-scanner/internal/suffix"
	"github.com/herrfennessey/ah-scanner/internal/upgrade"
	"net/http"
	"strconv"

	"github.com/herrfennessey/ah-scanner/internal/report"
)

// tooltipOut is what the hover tooltip shows: Blizzard's preview_item (every in-game tooltip
// line) plus this house's current prices.
type tooltipOut struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Quality   int             `json:"quality"`
	Icon      string          `json:"icon"`
	Preview   json.RawMessage `json:"preview"` // null until the collector has looked the item up
	House     string          `json:"house"`
	Min       int64           `json:"min"`
	Fair      int64           `json:"fair"`
	Available int64           `json:"available"`
	Hint      *report.Hint    `json:"hint,omitempty"`
	// Suffix variant: the in-game name and the suffix stats the preview leaves out.
	Variant     string               `json:"variant,omitempty"`
	SuffixName  string               `json:"suffix_name,omitempty"`
	SuffixStats []upgrade.SuffixLine `json:"suffix_stats,omitempty"`
	SuffixExact bool                 `json:"suffix_exact,omitempty"`
	SuffixOther int                  `json:"suffix_other,omitempty"`
}

func (s *Server) tooltip(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	h, err := s.houseParam(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	out := tooltipOut{ID: id, House: h.Faction}
	var tt sql.NullString
	var ilvl int
	err = s.Store.DB.QueryRowContext(r.Context(), `SELECT COALESCE(name,''), COALESCE(quality,1), COALESCE(icon,''), tooltip, COALESCE(item_level,0) FROM item WHERE id=?`, id).
		Scan(&out.Name, &out.Quality, &out.Icon, &tt, &ilvl)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, errors.New("this item has not been seen on the auction house"))
		return
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	if tt.Valid {
		out.Preview = json.RawMessage(tt.String)
	}
	if out.Name == "" {
		out.Name = "Item #" + strconv.FormatInt(id, 10)
	}
	out.Variant = r.URL.Query().Get("variant")
	if out.Variant != "" {
		out.SuffixName = suffix.Label(out.Name, out.Variant)
		var inv struct {
			InvType struct {
				Type string `json:"type"`
			} `json:"inventory_type"`
		}
		json.Unmarshal(out.Preview, &inv)
		out.SuffixStats, out.SuffixExact, out.SuffixOther = upgrade.SuffixLines(out.Variant, ilvl, out.Quality, inv.InvType.Type)
	}
	rows, err := s.buildRows(r.Context(), h.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	for _, row := range rows {
		if row.ID == id && row.Variant == out.Variant {
			out.Min, out.Fair, out.Available, out.Hint = row.Min, row.Fair, row.Available, row.Hint
			break
		}
	}
	writeJSON(w, out)
}
