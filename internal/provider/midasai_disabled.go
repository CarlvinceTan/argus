//go:build !midas

package provider

import (
	"errors"
	"github.com/carlvincetan/argus/internal/config"
)

func buildMidas(config.Config, bool) (DecisionProvider, VisionProvider, error) {
	return nil, nil, errors.New("midasai requires the midas build tag and an explicitly provisioned Midas module")
}
