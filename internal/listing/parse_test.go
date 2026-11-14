package listing

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func parseFile(t *testing.T, name string, kind Kind) ([]Listing, error) {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return ParseAll(f, kind)
}

func TestParseGearGolden(t *testing.T) {
	got, err := parseFile(t, "gear.json", Gear)
	if err != nil {
		t.Fatal(err)
	}
	want := []Listing{
		{AuctionID: 1001, ItemID: 4304, Quantity: 20, UnitPrice: 600, Buyout: 12000, Bid: 9000, TimeLeft: VeryLong},
		{AuctionID: 1002, ItemID: 15215, Variant: "r1101", Quantity: 1, UnitPrice: 80000, Buyout: 80000, Bid: 50000, TimeLeft: Long},
		{AuctionID: 1003, ItemID: 2589, Quantity: 5, UnitPrice: 0, Bid: 500, TimeLeft: Short},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseCommodityGolden(t *testing.T) {
	got, err := parseFile(t, "commodity.json", Commodity)
	if err != nil {
		t.Fatal(err)
	}
	want := []Listing{
		{AuctionID: 2001, ItemID: 7069, Quantity: 10, UnitPrice: 15000, Buyout: 150000, TimeLeft: Long},
		{AuctionID: 2002, ItemID: 7068, Quantity: 4, UnitPrice: 22000, Buyout: 88000, TimeLeft: Medium},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseIgnoresUnknownFields(t *testing.T) {
	got, err := parseFile(t, "unknown_field.json", Commodity)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AuctionID != 3001 || got[0].UnitPrice != 100 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMissingUnitPriceFails(t *testing.T) {
	_, err := parseFile(t, "missing_unit_price.json", Commodity)
	if err == nil || !strings.Contains(err.Error(), "unit_price") {
		t.Fatalf("want missing unit_price error, got %v", err)
	}
}

func TestParseTruncatedFails(t *testing.T) {
	if _, err := parseFile(t, "truncated.json", Commodity); err == nil {
		t.Fatal("want error on truncated body")
	}
}

func TestParseNoAuctionsArray(t *testing.T) {
	_, err := ParseAll(strings.NewReader(`{"items":[]}`), Commodity)
	if err != ErrNoAuctions {
		t.Fatalf("got %v", err)
	}
}

func TestVariantBonusListsSorted(t *testing.T) {
	got, err := ParseAll(strings.NewReader(`{"auctions":[{"id":1,"item":{"id":5,"bonus_lists":[30,10]},"buyout":10,"quantity":1,"time_left":"LONG"}]}`), Gear)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Variant != "b10:30" {
		t.Fatalf("variant %q", got[0].Variant)
	}
}
