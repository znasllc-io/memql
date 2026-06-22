package telephony

import (
	"context"
	"errors"
	"testing"
)

type stubCarrier struct{ name string }

func (s stubCarrier) Name() string { return s.name }
func (s stubCarrier) SearchNumbers(context.Context, NumberQuery) ([]Number, error) {
	return nil, nil
}
func (s stubCarrier) BuyNumber(context.Context, string) (Number, error) { return Number{}, nil }
func (s stubCarrier) ReleaseNumber(context.Context, string) error       { return nil }
func (s stubCarrier) ConfigureInbound(context.Context, string, string) error {
	return nil
}
func (s stubCarrier) ListNumbers(context.Context) ([]Number, error) { return nil, nil }

func TestRegisterAndSelect(t *testing.T) {
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)

	RegisterCarrier("telnyx", func() (CarrierProvider, error) { return stubCarrier{"telnyx"}, nil })
	RegisterCarrier("skyetel", func() (CarrierProvider, error) { return stubCarrier{"skyetel"}, nil })

	// Empty name falls back to the default carrier.
	c, err := SelectCarrier("")
	if err != nil {
		t.Fatalf("SelectCarrier(\"\"): %v", err)
	}
	if c.Name() != DefaultCarrier {
		t.Fatalf("default carrier = %q, want %q", c.Name(), DefaultCarrier)
	}

	// Explicit selection wins.
	c, err = SelectCarrier("skyetel")
	if err != nil {
		t.Fatalf("SelectCarrier(skyetel): %v", err)
	}
	if c.Name() != "skyetel" {
		t.Fatalf("got %q, want skyetel", c.Name())
	}

	// Registry lists carriers sorted.
	got := Carriers()
	if len(got) != 2 || got[0] != "skyetel" || got[1] != "telnyx" {
		t.Fatalf("Carriers() = %v, want [skyetel telnyx]", got)
	}
}

func TestSelectUnknownCarrier(t *testing.T) {
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)
	RegisterCarrier("telnyx", func() (CarrierProvider, error) { return stubCarrier{"telnyx"}, nil })

	_, err := SelectCarrier("nope")
	if err == nil {
		t.Fatal("expected error for unknown carrier")
	}
}

func TestFactoryErrorPropagates(t *testing.T) {
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)
	sentinel := errors.New("no creds")
	RegisterCarrier("telnyx", func() (CarrierProvider, error) { return nil, sentinel })

	_, err := SelectCarrier("telnyx")
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want sentinel", err)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)
	RegisterCarrier("telnyx", func() (CarrierProvider, error) { return stubCarrier{"telnyx"}, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	RegisterCarrier("telnyx", func() (CarrierProvider, error) { return stubCarrier{"telnyx"}, nil })
}
