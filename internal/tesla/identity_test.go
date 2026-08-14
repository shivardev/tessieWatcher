package tesla

import "testing"

// Test cases lifted directly from TeslaMate's Vehicle.identify/1
// (lib/teslamate/vehicles/vehicle.ex) match clauses, to pin exact
// parity rather than just "a reasonable model name".
func TestIdentifyVehicle(t *testing.T) {
	cases := []struct {
		name          string
		carType       string
		trimBadging   string
		vin           string
		wantModel     string
		wantTrim      string
		wantMarketing string
	}{
		{"Model S LR", "lychee", "100D", "", "S", "100D", "LR"},
		{"Model S Plaid", "lychee", "P100D", "", "S", "P100D", "Plaid"},
		{"Model S refresh LR+", "models2", "100D", "", "S", "100D", "LR+"},
		{"Model 3 Performance", "model3", "P74D", "", "3", "P74D", "LR AWD Performance"},
		{"Model 3 LR AWD", "model3", "74D", "", "3", "74D", "LR AWD"},
		{"Model 3 LR RWD", "model3", "74", "", "3", "74", "LR"},
		{"Model 3 MR", "model3", "62", "", "3", "62", "MR"},
		{"Model 3 base pre-2022 SR+", "model3", "50", "5YJ3E1EA1MF000001", "3", "50", "SR+"}, // 10th char 'M' = 2021
		{"Model 3 base 2022+ RWD", "model3", "50", "5YJ3E1EA1NF000001", "3", "50", "RWD"},    // 10th char 'N' = 2022
		{"Model X LR", "tamarind", "100D", "", "X", "100D", "LR"},
		{"Model X Plaid", "tamarind", "P100D", "", "X", "P100D", "Plaid"},
		{"Model Y Performance", "modely", "P74D", "", "Y", "P74D", "LR AWD Performance"},
		{"Model Y LR AWD", "modely", "74D", "", "Y", "74D", "LR AWD"},
		{"Model Y LR", "modely", "74", "", "Y", "74", "LR"},
		{"Model Y SR", "modely", "50", "", "Y", "50", "SR"},
		{"unknown car_type", "roadster2", "", "", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotTrim, gotMarketing := IdentifyVehicle(tc.carType, tc.trimBadging, tc.vin)
			if gotModel != tc.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tc.wantModel)
			}
			if gotTrim != tc.wantTrim {
				t.Errorf("trimBadging = %q, want %q", gotTrim, tc.wantTrim)
			}
			if gotMarketing != tc.wantMarketing {
				t.Errorf("marketingName = %q, want %q", gotMarketing, tc.wantMarketing)
			}
		})
	}
}

func TestModel3BaseTrimByVinYear(t *testing.T) {
	// VIN position 10 (0-indexed 9) encodes model year: 'N' = 2022,
	// 'M' = 2021. 2022+ -> "RWD", earlier -> "SR+".
	vin2022 := "5YJ3E1EANNF000001" // 10th char 'N' -> 2022
	vin2021 := "5YJ3E1EANMF000001" // 10th char 'M' -> 2021

	if got := model3BaseTrim(vin2022); got != "RWD" {
		t.Errorf("2022 VIN: got %q, want RWD", got)
	}
	if got := model3BaseTrim(vin2021); got != "SR+" {
		t.Errorf("2021 VIN: got %q, want SR+", got)
	}
	if got := model3BaseTrim("too-short"); got != "SR+" {
		t.Errorf("malformed VIN should fall back to SR+, got %q", got)
	}
}
