package tesla

import "strings"

// vinModelYears mirrors TeslaMate's @vin_model_years map
// (lib/teslamate/vehicles/vehicle.ex) exactly: VIN position 10 (0-indexed
// 9) is the standard NHTSA model-year code.
var vinModelYears = map[byte]int{
	'A': 2010, 'B': 2011, 'C': 2012, 'D': 2013, 'E': 2014, 'F': 2015,
	'G': 2016, 'H': 2017, 'J': 2018, 'K': 2019, 'L': 2020, 'M': 2021,
	'N': 2022, 'P': 2023, 'R': 2024, 'S': 2025, 'T': 2026, 'V': 2027,
	'W': 2028, 'X': 2029, 'Y': 2030,
	'1': 2031, '2': 2032, '3': 2033, '4': 2034, '5': 2035, '6': 2036,
	'7': 2037, '8': 2038, '9': 2039,
}

// vinModelYear returns the model year encoded in VIN position 10, or 0
// if vin isn't the standard 17 characters.
func vinModelYear(vin string) int {
	if len(vin) != 17 {
		return 0
	}
	return vinModelYears[vin[9]]
}

// model3BaseTrim disambiguates the Model 3 base ("50") trim code, which
// Tesla reused for both "Standard Range Plus" (pre-2022) and "RWD"
// (2022+) - TeslaMate tells them apart via VIN model year.
func model3BaseTrim(vin string) string {
	if y := vinModelYear(vin); y >= 2022 {
		return "RWD"
	}
	return "SR+"
}

// IdentifyVehicle derives the normalized single-letter model code and
// human marketing/trim name from the Owner API's raw vehicle_config
// fields, exactly reproducing TeslaMate's Vehicle.identify/1
// (lib/teslamate/vehicles/vehicle.ex) - neither of these is a literal
// API field TeslaMate stores it reports; TeslaMate itself derives both
// from car_type/trim_badging/VIN via a hardcoded lookup table, so
// "identical to TeslaMate" here means reproducing that derivation, not
// just copying a raw field.
//
// carType is the raw vehicle_config.car_type value (e.g. "model3",
// "modely", "lychee", "models2", "tamarind"). trimBadging is the raw
// vehicle_config.trim_badging value (e.g. "74D", "P100D"). Returns
// (model, trimBadgingUpper, marketingName); any of the three can come
// back empty if it can't be determined, same as TeslaMate returning nil.
func IdentifyVehicle(carType, trimBadging, vin string) (model, trimUpper, marketingName string) {
	trimUpper = strings.ToUpper(trimBadging)

	switch t := strings.ToLower(carType); {
	case strings.HasPrefix(t, "models"): // covers both "models" and "models2"
		model = "S"
	case strings.HasPrefix(t, "model3"):
		model = "3"
	case strings.HasPrefix(t, "modelx"):
		model = "X"
	case strings.HasPrefix(t, "modely"):
		model = "Y"
	case t == "lychee":
		model = "S"
	case t == "tamarind":
		model = "X"
	}

	switch {
	case model == "S" && trimUpper == "100D" && carType == "lychee":
		marketingName = "LR"
	case model == "S" && trimUpper == "P100D" && carType == "lychee":
		marketingName = "Plaid"
	case model == "S" && trimUpper == "100D" && carType == "models2":
		marketingName = "LR+"
	case model == "3" && trimUpper == "P74D":
		marketingName = "LR AWD Performance"
	case model == "3" && trimUpper == "74D":
		marketingName = "LR AWD"
	case model == "3" && trimUpper == "74":
		marketingName = "LR"
	case model == "3" && trimUpper == "62":
		marketingName = "MR"
	case model == "3" && trimUpper == "50":
		marketingName = model3BaseTrim(vin)
	case model == "X" && trimUpper == "100D" && carType == "tamarind":
		marketingName = "LR"
	case model == "X" && trimUpper == "P100D" && carType == "tamarind":
		marketingName = "Plaid"
	case model == "Y" && trimUpper == "P74D":
		marketingName = "LR AWD Performance"
	case model == "Y" && trimUpper == "74D":
		marketingName = "LR AWD"
	case model == "Y" && trimUpper == "74":
		marketingName = "LR"
	case model == "Y" && trimUpper == "50":
		marketingName = "SR"
	}

	return model, trimUpper, marketingName
}
