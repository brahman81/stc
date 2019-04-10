package stc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"github.com/xdrpp/stc/stcdetail"
	"github.com/xdrpp/stc/stx"
)

// A communication error with horizon
type horizonFailure string
func (e horizonFailure) Error() string {
	return string(e)
}

const badHorizonURL horizonFailure = "Missing or invalid horizon URL"

func get(net *StellarNet, query string) ([]byte, error) {
	if net.Horizon == "" {
		return nil, badHorizonURL
	}
	resp, err := http.Get(net.Horizon + query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

type HorizonSigner struct {
	Key    string
	Weight uint32
}
type HorizonAccountEntry struct {
	Sequence   json.Number
	Thresholds struct {
		Low_threshold  uint8
		Med_threshold  uint8
		High_threshold uint8
	}
	Signers []HorizonSigner
}

// Convert a json.Number to an int64 by scanning the textual
// representation of the number.  (The simpler approach of going
// through a double risks losing precision.)
func JsonNumberToI64(n json.Number) (int64, error) {
	val := int64(-1)
	if _, err := fmt.Sscan(n.String(), &val); err != nil {
		return -1, err
	}
	return val, nil
}

// Return the next sequence number (1 + Sequence) as an int64 (or 0 if
// an invalid sequence number was returned by horizon).
func (ae *HorizonAccountEntry) NextSeq() int64 {
	if val, err := JsonNumberToI64(ae.Sequence); err != nil || val + 1 <= 0 {
		return 0
	} else {
		return val + 1
	}
}

// Fetch the sequence number and signers of an account over the
// network.
func (net *StellarNet) GetAccountEntry(acct string) (
	*HorizonAccountEntry, error) {
	if body, err := get(net, "accounts/" + acct); err != nil {
		return nil, err
	} else {
		var ae HorizonAccountEntry
		if err = json.Unmarshal(body, &ae); err != nil {
			return nil, err
		}
		return &ae, nil
	}
}

func (net *StellarNet) GetNetworkId() string {
	if net.NetworkId != "" {
		return net.NetworkId
	}
	if body, err := get(net, "/"); err != nil {
		return ""
	} else {
		var np struct { Network_passphrase string }
		if err = json.Unmarshal(body, &np); err != nil {
			return ""
		}
		net.NetworkId = np.Network_passphrase
		return net.NetworkId
	}
}

var feeSuffix string = "_accepted_fee"
type feePercentile = struct {
	Percentile int
	Fee uint32
}

// Go representation of the Horizon Fee Stats structure response.  The
// fees are per operation in a transaction, and the individual fields
// are documented here:
// https://www.stellar.org/developers/horizon/reference/endpoints/fee-stats.html
type FeeStats struct {
	Last_ledger uint64
	Last_ledger_base_fee uint32
	Ledger_capacity_usage float64
	Min_accepted_fee uint32
	Mode_accepted_fee uint32
	Percentiles []struct {
		Percentile int
		Fee uint32
	}
}

func (fs FeeStats) String() string {
	out := strings.Builder{}
	rv := reflect.ValueOf(&fs).Elem()
	tp := rv.Type()
	for i := 0; i < tp.NumField(); i++ {
		field := tp.Field(i).Name
		if field != "Percentiles" {
			fmt.Fprintf(&out, "%24s: %v\n", strings.ToLower(field),
				rv.Field(i).Interface())
		}
	}
	for i := range fs.Percentiles {
		fmt.Fprintf(&out, "%9d_percentile_fee: %d\n",
			fs.Percentiles[i].Percentile,
			fs.Percentiles[i].Fee)
	}
	return out.String()
}

// Conservatively returns a fee that is a known fee for the target or
// the closest higher known percentile.  Does not interpolate--e.g.,
// if you ask for the 51st percentile but only the 50th and 60th are
// known, returns the 60th percentile.  Never returns a value less
// than the base fee.
func (fs *FeeStats) Percentile(target int) uint32 {
	var fee uint32
	if len(fs.Percentiles) > 0 {
		fee = 1 + fs.Percentiles[len(fs.Percentiles)-1].Fee
	}
	for lo, hi := 0, len(fs.Percentiles); lo < hi; {
		n := (lo + hi) / 2
		p := &fs.Percentiles[n]
		if p.Percentile == target {
			fee = p.Fee
			break
		} else if p.Percentile > target {
			if fee > p.Fee {
				fee = p.Fee
			}
			hi = n
		} else {
			lo = n + 1
		}
	}
	if fee < fs.Last_ledger_base_fee {
		fee = fs.Last_ledger_base_fee
	}
	return fee
}

func capitalize(s string) string {
        if len(s) > 0 && s[0] >= 'a' && s[0] <= 'z' {
                return string(s[0] &^ 0x20) + s[1:]
        }
        return s
}

func getU32(i interface{}) (uint32, error) {
	// Annoyingly, Horizion aleays returns strings instead of numbers
	// for the /fee_stats endpoint.  Because this behavior is
	// annoying, we want to be prepared for it to change, which is why
	// we Sprint and then Parse.
	n, err := strconv.ParseUint(fmt.Sprint(i), 10, 32)
	return uint32(n), err
}

// Queries the network for the latest fee statistics.
func (net *StellarNet) GetFeeStats() (*FeeStats, error) {
	body, err := get(net, "fee_stats")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]interface{}
	if err = dec.Decode(&obj); err != nil {
		return nil, err
	}

	var fs FeeStats
	rv := reflect.ValueOf(&fs).Elem()
	for k := range obj {
		if strings.HasSuffix(k, feeSuffix) &&
			k[0] == 'p' && k[1] >= '0' || k[1] <= '9' {
			if p, err := getU32(k[1:len(k)-len(feeSuffix)]); err == nil {
				if fee, err := getU32(obj[k]); err == nil {
					fs.Percentiles = append(fs.Percentiles, feePercentile{
						Percentile: int(p),
						Fee: fee,
					})
				}
			}
			continue
		}
		capk := capitalize(k)
		if capk == "Percentiles" {
			continue // Server is messing with us
		}
		switch field, s := rv.FieldByName(capk), fmt.Sprint(obj[k]);
		field.Kind() {
		case reflect.Uint32:
			if v, err := strconv.ParseUint(s, 10, 32); err == nil {
				field.SetUint(v)
			}
		case reflect.Uint64:
			if v, err := strconv.ParseUint(s, 10, 64); err == nil {
				field.SetUint(v)
			}
		case reflect.Float64:
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				field.SetFloat(v)
			}
		}
	}
	if fs.Min_accepted_fee == 0 || fs.Last_ledger_base_fee == 0 ||
		len(fs.Percentiles) == 0 {
		// Something's wrong; don't return garbage
		return nil, horizonFailure("Garbled fee_stats")
	}

	sort.Slice(fs.Percentiles, func(i, j int) bool {
		return fs.Percentiles[i].Percentile < fs.Percentiles[j].Percentile
	})
	return &fs, nil
}

// Fetch the latest ledger header over the network.
func (net *StellarNet) GetLedgerHeader() (*LedgerHeader, error) {
	body, err := get(net, "ledgers?limit=1&order=desc")
	if err != nil {
		return nil, err
	}

	var lhx struct {
		Embedded struct {
			Records []struct {
				Header_xdr string
			}
		} `json:"_embedded"`
	}
	if err = json.Unmarshal(body, &lhx); err != nil {
		return nil, err
	} else if len(lhx.Embedded.Records) == 0 {
		return nil, horizonFailure("Horizon returned no ledgers")
	}

	ret := &LedgerHeader{}
	if err = stcdetail.XdrFromBase64(ret, lhx.Embedded.Records[0].Header_xdr);
	err != nil {
		return nil, err
	}
	return ret, nil
}

// An error representing the failure of a transaction submitted to the
// Stellar network
type TxFailure struct {
	*TransactionResult
}
func (e TxFailure) Error() string {
	switch e.Result.Code {
	case stx.TxSUCCESS:
		return "all operations succeeded"
	case stx.TxFAILED:
		out := strings.Builder{}
		fmt.Println(&out, "one of the operations failed (none were applied)")
		stcdetail.XdrToTxrep(&out, &e.Result)
		return out.String()
	case stx.TxTOO_EARLY:
		return "ledger closeTime before minTime"
	case stx.TxTOO_LATE:
		return "ledger closeTime after maxTime"
	case stx.TxMISSING_OPERATION:
		return "no operation was specified"
	case stx.TxBAD_SEQ:
		return "sequence number does not match source account"
	case stx.TxBAD_AUTH:
		return "too few valid signatures / wrong network"
	case stx.TxINSUFFICIENT_BALANCE:
		return "fee would bring account below reserve"
	case stx.TxNO_ACCOUNT:
		return "source account not found"
	case stx.TxINSUFFICIENT_FEE:
		return "fee is too small"
	case stx.TxBAD_AUTH_EXTRA:
		return "unused signatures attached to transaction"
	case stx.TxINTERNAL_ERROR:
		return "an unknown error occured"
	default:
		return e.Result.Code.String()
	}
}

// Post a new transaction to the network.
func (net *StellarNet) Post(e *TransactionEnvelope) (
	*TransactionResult, error) {
	if net.Horizon == "" {
		return nil, badHorizonURL
	}
	tx := stcdetail.XdrToBase64(e)
	resp, err := http.PostForm(net.Horizon + "/transactions",
		url.Values{"tx": {tx}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	js := json.NewDecoder(resp.Body)
	var res struct {
		Result_xdr string
		Extras     struct {
			Result_xdr string
		}
	}
	if err = js.Decode(&res); err != nil {
		return nil, err
	}
	if res.Result_xdr == "" {
		res.Result_xdr = res.Extras.Result_xdr
	}

	var ret TransactionResult
	if err = stcdetail.XdrFromBase64(&ret, res.Result_xdr); err != nil {
		return nil, err
	}
	if ret.Result.Code != stx.TxSUCCESS {
		return nil, TxFailure{&ret}
	}
	return &ret, nil
}