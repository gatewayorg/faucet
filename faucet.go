// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

// faucet is an Ether faucet backed by a light client.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"syscall"

	"github.com/sunvim/utils/log"
)

var (
	apiName  = flag.String("name", "Edge", "faucet name")
	apiPort  = flag.String("apiport", "8080", "Listener port for the HTTP API connection")
	apiAddr  = flag.String("apiaddr", "127.0.0.1", "Listener Address")
	apiHttps = flag.Bool("https", false, "https service flag")

	priKey       = flag.String("pri_key", "d57caa3e1da880fdef9d1c586c72d4ab99f0acccee6fb8b2e53dd6251c9c6cd5", "private key")
	key          = flag.String("key", "tls.key", "certificate key")
	crt          = flag.String("crt", "tls.crt", "certificate file")
	captchaToken = flag.String("captcha.token", "", "Recaptcha site key to authenticate client side")
	tiersFlag    = flag.Int("faucet.tiers", 2, "Number of funding tiers to enable (x3 time, x2.5 funds)")
	startFlag    = flag.Float64("faucet.start", 0.1, "Number of funding tiers to enable (x3 time, x2.5 funds)")
	UnitFlag     = flag.String("unit", "Edge", "token unit")
	payoutFlag   = flag.Float64("faucet.amount", 1.0, "Number of unit to pay out per user request")
	minutesFlag  = flag.Int("faucet.minutes", 1440, "Number of minutes to wait between funding rounds")
	rpc          = flag.String("rpc", "https://meta-ape-edge-testnet-01.ankr.com", "rpc url")
	chainID      = flag.Int64("chain_id", 100, "chain id")
)

var (
	// ether = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	ether = 1000_000_000_000_000_000
)

func main() {
	log.SetLevel(log.LevelInfo)
	log.SetLogPrefix("Faucet")
	setupRLimit()
	// Parse the flags and set up the logger to print everything requested
	flag.Parse()
	initFaucet()

	// Construct the payout tiers
	amounts := make([]string, *tiersFlag)
	periods := make([]string, *tiersFlag)
	for i := 0; i < *tiersFlag; i++ {
		// Calculate the amount for the next tier and format it
		amount := (*payoutFlag + float64(i)) * (*startFlag)
		amounts[i] = fmt.Sprintf("%s %s", strconv.FormatFloat(amount, 'f', -1, 64), *UnitFlag)
		if amount == 1 {
			amounts[i] = strings.TrimSuffix(amounts[i], "s")
		}
		// Calculate the period for the next tier and format it
		period := *minutesFlag * int(math.Pow(3, float64(i)))
		periods[i] = fmt.Sprintf("%d mins", period)
		if period%60 == 0 {
			period /= 60
			periods[i] = fmt.Sprintf("%d hours", period)

			if period%24 == 0 {
				period /= 24
				periods[i] = fmt.Sprintf("%d days", period)
			}
		}
		if period == 1 {
			periods[i] = strings.TrimSuffix(periods[i], "s")
		}
	}

	// Load up and render the faucet website
	tmpl, err := Asset("faucet.html")
	if err != nil {
		log.Fatal("Failed to load the faucet template", err)
	}
	website := new(bytes.Buffer)
	err = template.Must(template.New("").Parse(string(tmpl))).Execute(website, map[string]interface{}{
		"Name":      *apiName,
		"Amounts":   amounts,
		"Periods":   periods,
		"Recaptcha": *captchaToken,
	})
	if err != nil {
		log.Fatal("Failed to render the faucet template", err)
	}

	mux := &http.ServeMux{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(website.Bytes())
	})
	mux.HandleFunc("/api", OnWebsocket)

	address := strings.Join([]string{*apiAddr, ":", *apiPort}, "")
	log.Infof("service booting with %s \n", address)

	if !*apiHttps {
		http.ListenAndServe(address, mux)
	} else {
		http.ListenAndServeTLS(address, *key, *crt, mux)
	}

}

func setupRLimit() {
	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		panic(err)
	}
	rlimit.Cur = rlimit.Max - 100
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		panic(err)
	}
}
