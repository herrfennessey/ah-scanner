package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	meta, err := loadMeta(r.Context(), s.Store.DB)
	if err != nil {
		fail(w, 500, err)
		return
	}
	m, ok := meta[id]
	if !ok {
		fail(w, 404, errors.New("this item has not been seen on the auction house"))
		return
	}
	out := tooltipOut{ID: id, Name: m.name, Quality: m.quality, Icon: m.icon, House: h.Faction}
	if out.Name == "" {
		out.Name = "Item #" + strconv.FormatInt(id, 10)
	}
	if out.Preview, err = s.Store.ItemTooltip(r.Context(), id); err != nil {
		fail(w, 500, err)
		return
	}
	rows, err := s.buildRows(r.Context(), h.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	for _, row := range rows {
		if row.ID == id && row.Variant == "" {
			out.Min, out.Fair, out.Available = row.Min, row.Fair, row.Available
			break
		}
	}
	writeJSON(w, out)
}
