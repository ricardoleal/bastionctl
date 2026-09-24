package netcidr

import (
	"reflect"
	"testing"
)

func TestNormalizeUniqueCanonicalizesSortsAndDeduplicates(t *testing.T) {
	got, err := NormalizeUnique([]string{"10.20.1.2/16", "10.0.0.0/8", "10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "10.20.0.0/16"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeUnique() = %v, want %v", got, want)
	}
}

func TestNormalizeRejectsInvalidCIDR(t *testing.T) {
	if _, err := Normalize("10.0.0.1"); err == nil {
		t.Fatal("Normalize accepted an address without a prefix")
	}
}
