package api

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

var percentPattern = regexp.MustCompile(`^\d{1,3}\.\d{2}$`)

// percent reads "2.00" as hundredths of a percent, 200.
func percent(s, param string) (int64, error) {
	if !percentPattern.MatchString(s) {
		return 0, invalidRequest("parameter_invalid", param, "%s must be a percentage such as \"2.00\".", param)
	}
	whole, cents, _ := strings.Cut(s, ".")
	w, _ := strconv.ParseInt(whole, 10, 64)
	c, _ := strconv.ParseInt(cents, 10, 64)
	return w*100 + c, nil
}

func formatPercent(hundredths int64) string {
	return strconv.FormatInt(hundredths/100, 10) + "." + strconv.FormatInt(hundredths%100/10, 10) + strconv.FormatInt(hundredths%10, 10)
}

func pixOptionsParam(o *openapi.PixOptions) (*payments.PixOptions, error) {
	if o == nil {
		return nil, nil //nolint:nilnil // no options sent
	}
	out := &payments.PixOptions{}
	if o.ExpiresAfterSeconds != nil {
		out.ExpiresAfterSeconds = *o.ExpiresAfterSeconds
	}
	if o.DueDate == nil {
		if o.DaysAfterDue != nil || o.Payer != nil || o.Fine != nil || o.Interest != nil || o.Discount != nil {
			return nil, invalidRequest("parameter_invalid", "pix[due_date]", "days_after_due, payer, fine, interest and discount are for a charge with a due_date.")
		}
		return out, nil
	}
	due := &payments.PixDue{Date: o.DueDate.String(), DaysAfter: 30}
	if o.DaysAfterDue != nil {
		due.DaysAfter = *o.DaysAfterDue
	}
	if o.Payer != nil {
		due.PayerName, due.PayerTaxID = o.Payer.Name, o.Payer.TaxId
	}
	if err := fineParam(o, due); err != nil {
		return nil, err
	}
	if o.Interest != nil {
		p, err := percent(o.Interest.MonthlyPercent, "pix[interest][monthly_percent]")
		if err != nil {
			return nil, err
		}
		due.InterestMonthlyPercent = p
	}
	if o.Discount != nil {
		due.DiscountAmount, due.DiscountUntil = o.Discount.Amount, o.Discount.Until.String()
	}
	out.Due = due
	return out, nil
}

func fineParam(o *openapi.PixOptions, due *payments.PixDue) error {
	if o.Fine == nil {
		return nil
	}
	if o.Fine.Amount != nil {
		due.FineAmount = *o.Fine.Amount
	}
	if o.Fine.Percent != nil {
		p, err := percent(*o.Fine.Percent, "pix[fine][percent]")
		if err != nil {
			return err
		}
		due.FinePercent = p
	}
	if due.FineAmount == 0 && due.FinePercent == 0 {
		return invalidRequest("parameter_invalid", "pix[fine]", "pix[fine] needs an amount or a percent.")
	}
	return nil
}

// allocate points a pointer field at a new value and returns it: many generated types
// nest anonymous structs, which cannot be named to be allocated.
func allocate[T any](field **T) *T {
	*field = new(T)
	return *field
}

func pixOptionsJSON(o *payments.PixOptions) *openapi.PixOptions {
	if o == nil {
		return nil
	}
	out := &openapi.PixOptions{}
	if o.ExpiresAfterSeconds != 0 {
		out.ExpiresAfterSeconds = &o.ExpiresAfterSeconds
	}
	d := o.Due
	if d == nil {
		return out
	}
	due := allocate(&out.DueDate)
	_ = due.UnmarshalText([]byte(d.Date))
	out.DaysAfterDue = &d.DaysAfter
	payer := allocate(&out.Payer)
	payer.Name, payer.TaxId = d.PayerName, d.PayerTaxID
	switch {
	case d.FineAmount > 0:
		allocate(&out.Fine).Amount = &d.FineAmount
	case d.FinePercent > 0:
		p := formatPercent(d.FinePercent)
		allocate(&out.Fine).Percent = &p
	}
	if d.InterestMonthlyPercent > 0 {
		allocate(&out.Interest).MonthlyPercent = formatPercent(d.InterestMonthlyPercent)
	}
	if d.DiscountAmount > 0 {
		discount := allocate(&out.Discount)
		discount.Amount = d.DiscountAmount
		_ = discount.Until.UnmarshalText([]byte(d.DiscountUntil))
	}
	return out
}
