package report

import (
	"encoding/json"

	"github.com/klauspost/compress/zstd"
)

// Flip inputs are ~14 MB of JSON a scan; zstd brings them to ~2 MB and decodes in ~15 ms, which
// makes keeping them for every scan (flip_history) affordable.

// InputsVersion is stored with every flip_history row; the backtest replays only rows built
// with the current one. Bump it whenever BuildFlipInputs computes anything differently, so
// inputs stored by older code are rebuilt instead of replayed.
const InputsVersion = "1"

// EncodeAll and DecodeAll are safe for concurrent use.
var (
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	zdec, _ = zstd.NewReader(nil)
)

// EncodeInputs marshals flip inputs to compressed JSON for flip_history.
func EncodeInputs(in *FlipInputs) ([]byte, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return CompressInputs(b), nil
}

// CompressInputs compresses already-marshalled flip inputs (the collector has the JSON at hand).
func CompressInputs(b []byte) []byte {
	return zenc.EncodeAll(b, make([]byte, 0, len(b)/6))
}

// DecodeInputs reverses EncodeInputs.
func DecodeInputs(z []byte) (*FlipInputs, error) {
	b, err := zdec.DecodeAll(z, nil)
	if err != nil {
		return nil, err
	}
	var in FlipInputs
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	return &in, nil
}
