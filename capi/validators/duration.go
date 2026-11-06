// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package validators

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// DurationValidator validates that a string is a positive Go duration
// (`90s`, `10m`, `1h30m`).
func DurationValidator() validator.String {
	return &durationValidator{}
}

type durationValidator struct{}

func (v durationValidator) Description(ctx context.Context) string {
	return "value must be a positive Go duration such as `10m`"
}

func (v durationValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v durationValidator) ValidateString(
	ctx context.Context,
	req validator.StringRequest,
	resp *validator.StringResponse,
) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	d, err := time.ParseDuration(value)
	if err != nil {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid Duration",
			fmt.Sprintf("Value %q is not a Go duration: %s.", value, err),
		)
		return
	}
	if d <= 0 {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid Duration",
			fmt.Sprintf("Value %q must be longer than zero.", value),
		)
	}
}
