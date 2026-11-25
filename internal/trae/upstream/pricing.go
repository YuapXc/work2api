package upstream

import (
	"encoding/json"
	"fmt"
	"math"
)

// Display-only catalog data; never used as a token price or routing permission.
type ModelPrice struct {
	Product    string           `json:"product"`
	Rate       *float64         `json:"rate"`
	Promotions []ModelPromotion `json:"promotions,omitempty"`
	Stale      bool             `json:"stale,omitempty"`
}
type ModelPromotion struct {
	Label       string            `json:"label"`
	Description string            `json:"description,omitempty"`
	Region      string            `json:"region"`
	Partial     bool              `json:"partial,omitempty"`
	Timezone    string            `json:"timezone,omitempty"`
	Weekly      []PromotionWindow `json:"weekly,omitempty"`
}
type PromotionWindow struct {
	Weekdays []int  `json:"weekdays"`
	Start    string `json:"start"`
	End      string `json:"end"`
}

func catalogObject(raw json.RawMessage) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return json.RawMessage(text)
	}
	return raw
}

type modelDisplay struct {
	MaxMode                bool            `json:"max_mode"`
	DisplayName            string          `json:"display_name"`
	Multimodal             *bool           `json:"multimodal"`
	Capability             string          `json:"model_capability"`
	ToolResponseMultimodal *bool           `json:"tool_response_multimodal"`
	Invisible              bool            `json:"is_invisible_to_user"`
	Custom                 bool            `json:"is_custom_model"`
	Builder                json.RawMessage `json:"builder"`
}

func (d *modelDisplay) UnmarshalJSON(raw []byte) error {
	type plain modelDisplay
	return json.Unmarshal(catalogObject(raw), (*plain)(d))
}
func builderPresent(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v != ""
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	case float64:
		return v != 0
	}
	return false
}
func validRate(rate *float64) bool {
	return rate != nil && !math.IsNaN(*rate) && !math.IsInf(*rate, 0) && *rate >= 0
}

func parseModelPrice(raw json.RawMessage, product string) ModelPrice {
	price := ModelPrice{Product: product}
	var contact struct {
		Rate struct {
			Enable bool `json:"enable"`
			Data   struct {
				Rate *float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
		Discount struct {
			Enable bool `json:"enable"`
			Data   struct {
				Current struct {
					Kind   string   `json:"discount_type"`
					Before *float64 `json:"before_consumption_rate"`
					Rate   *float64 `json:"consumption_rate"`
				} `json:"current"`
				OffPeak struct {
					Zone    string `json:"time_zone"`
					Windows []struct {
						Days  []int `json:"weekdays"`
						Start int   `json:"start_minute"`
						End   int   `json:"end_minute"`
					} `json:"time_windows"`
				} `json:"off_peak"`
			} `json:"data"`
		} `json:"activity_discount"`
	}
	if json.Unmarshal(catalogObject(raw), &contact) != nil {
		return price
	}
	if contact.Rate.Enable && validRate(contact.Rate.Data.Rate) {
		rate := *contact.Rate.Data.Rate
		price.Rate = &rate
	}
	if !contact.Discount.Enable {
		return price
	}
	d := contact.Discount.Data
	p := ModelPromotion{Label: "活动优惠", Region: product}
	switch d.Current.Kind {
	case "off_peak":
		p.Label = "错峰优惠"
	case "member":
		p.Label = "会员优惠"
	}
	if validRate(d.Current.Before) && validRate(d.Current.Rate) {
		p.Description = fmt.Sprintf("基础倍率 ×%g，目录当前倍率 ×%g（已含优惠）。", *d.Current.Before, *d.Current.Rate)
	}
	if len(d.OffPeak.Windows) > 0 {
		p.Timezone = d.OffPeak.Zone
		for _, w := range d.OffPeak.Windows {
			if len(w.Days) == 0 || w.Start < 0 || w.Start >= 1440 || w.End < 0 || w.End > 1440 || w.Start == w.End {
				continue
			}
			days := []int{}
			seen := map[int]bool{}
			for _, day := range w.Days {
				if day >= 1 && day <= 7 && !seen[day] {
					days = append(days, day)
					seen[day] = true
				}
			}
			if len(days) == 0 {
				continue
			}
			p.Weekly = append(p.Weekly, PromotionWindow{Weekdays: days, Start: fmt.Sprintf("%02d:%02d", w.Start/60, w.Start%60), End: fmt.Sprintf("%02d:%02d", w.End/60, w.End%60)})
		}
	}
	price.Promotions = []ModelPromotion{p}
	return price
}
