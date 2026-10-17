package huly

import (
	"fmt"
	"math/big"
	"strings"
)

// This file ports the LexoRank arithmetic Huly orders issues with
// (@hcengineering/rank's makeRank over the lexorank package): a rank is
// "<bucket>|<base-36 decimal>", where the decimal has six integer digits
// and ':' as its radix point, for example "0|hzzzzz:".

const rankDigits = "0123456789abcdefghijklmnopqrstuvwxyz"

var (
	rankBase      = big.NewInt(36)
	rankOne       = big.NewInt(1)
	rankMinDec    = lexoDecimal{mag: big.NewInt(0)}
	rankMaxDec    = mustParseDecimal("zzzzzz")
	rankEight     = mustParseDecimal("8")
	rankInitMin   = mustParseDecimal("100000")
	rankInitMax   = mustParseDecimal("y00000")
	rankHalf      = lexoDecimal{mag: big.NewInt(18), sig: 1}
	rankBucketSep = "|"
)

// lexoDecimal is mag / 36^sig, non-negative, with no trailing zero digit
// below the radix point.
type lexoDecimal struct {
	mag *big.Int
	sig int
}

func pow36(n int) *big.Int {
	return new(big.Int).Exp(rankBase, big.NewInt(int64(n)), nil)
}

func makeDecimal(mag *big.Int, sig int) lexoDecimal {
	if mag.Sign() == 0 {
		return lexoDecimal{mag: new(big.Int), sig: 0}
	}
	m := new(big.Int).Set(mag)
	r := new(big.Int)
	for sig > 0 {
		q, rem := new(big.Int).QuoRem(m, rankBase, r)
		if rem.Sign() != 0 {
			break
		}
		m = q
		sig--
	}
	return lexoDecimal{mag: m, sig: sig}
}

func parseDecimal(s string) (lexoDecimal, error) {
	if strings.Count(s, ":") > 1 {
		return lexoDecimal{}, fmt.Errorf("more than one ':' in %q", s)
	}
	intPart, frac, _ := strings.Cut(s, ":")
	digits := intPart + frac
	mag := new(big.Int)
	for _, r := range digits {
		d := strings.IndexRune(rankDigits, r)
		if d < 0 {
			return lexoDecimal{}, fmt.Errorf("invalid rank digit %q in %q", r, s)
		}
		mag.Mul(mag, rankBase)
		mag.Add(mag, big.NewInt(int64(d)))
	}
	return makeDecimal(mag, len(frac)), nil
}

func mustParseDecimal(s string) lexoDecimal {
	d, err := parseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

func (d lexoDecimal) aligned(o lexoDecimal) (*big.Int, *big.Int, int) {
	a, b := new(big.Int).Set(d.mag), new(big.Int).Set(o.mag)
	sig := d.sig
	switch {
	case d.sig < o.sig:
		a.Mul(a, pow36(o.sig-d.sig))
		sig = o.sig
	case d.sig > o.sig:
		b.Mul(b, pow36(d.sig-o.sig))
	}
	return a, b, sig
}

func (d lexoDecimal) add(o lexoDecimal) lexoDecimal {
	a, b, sig := d.aligned(o)
	return makeDecimal(a.Add(a, b), sig)
}

func (d lexoDecimal) multiply(o lexoDecimal) lexoDecimal {
	return makeDecimal(new(big.Int).Mul(d.mag, o.mag), d.sig+o.sig)
}

func (d lexoDecimal) cmp(o lexoDecimal) int {
	a, b, _ := d.aligned(o)
	return a.Cmp(b)
}

func (d lexoDecimal) floor() *big.Int {
	return new(big.Int).Quo(d.mag, pow36(d.sig))
}

func (d lexoDecimal) ceil() *big.Int {
	f := d.floor()
	if new(big.Int).Mul(f, pow36(d.sig)).Cmp(d.mag) == 0 {
		return f
	}
	return f.Add(f, rankOne)
}

// setScale truncates to nsig digits below the radix point, adding one
// unit at the new scale when ceiling is set, as lexorank does.
func (d lexoDecimal) setScale(nsig int, ceiling bool) lexoDecimal {
	if nsig >= d.sig {
		return d
	}
	if nsig < 0 {
		nsig = 0
	}
	m := new(big.Int).Quo(d.mag, pow36(d.sig-nsig))
	if ceiling {
		m.Add(m, rankOne)
	}
	return makeDecimal(m, nsig)
}

// format renders the decimal with at least six integer digits and no
// trailing zero, the way a LexoRank prints it.
func (d lexoDecimal) format() string {
	digits := d.mag.Text(36)
	for len(digits) < d.sig+1 {
		digits = "0" + digits
	}
	intPart := digits[:len(digits)-d.sig]
	frac := digits[len(digits)-d.sig:]
	for len(intPart) < 6 {
		intPart = "0" + intPart
	}
	return intPart + ":" + strings.TrimRight(frac, "0")
}

func rankMid(left, right lexoDecimal) lexoDecimal {
	mid := left.add(right).multiply(rankHalf)
	scale := max(left.sig, right.sig)
	if mid.sig > scale {
		if down := mid.setScale(scale, false); down.cmp(left) > 0 {
			return down
		}
		if up := mid.setScale(scale, true); up.cmp(right) < 0 {
			return up
		}
	}
	return mid
}

func rankCheckMid(lbound, rbound, mid lexoDecimal) lexoDecimal {
	if lbound.cmp(mid) >= 0 || mid.cmp(rbound) >= 0 {
		return rankMid(lbound, rbound)
	}
	return mid
}

// rankBetween ports LexoRank.between: the shortest decimal strictly
// between oLeft and oRight.
func rankBetween(oLeft, oRight lexoDecimal) lexoDecimal {
	left, right := oLeft, oRight
	if oLeft.sig < oRight.sig {
		nLeft := oRight.setScale(oLeft.sig, false)
		if oLeft.cmp(nLeft) >= 0 {
			return rankMid(oLeft, oRight)
		}
		right = nLeft
	}
	if oLeft.sig > right.sig {
		nLeft := oLeft.setScale(right.sig, true)
		if nLeft.cmp(right) >= 0 {
			return rankMid(oLeft, oRight)
		}
		left = nLeft
	}
	for scale := left.sig; scale > 0; {
		nScale := scale - 1
		nLeft := left.setScale(nScale, true)
		nRight := right.setScale(nScale, false)
		c := nLeft.cmp(nRight)
		if c == 0 {
			return rankCheckMid(oLeft, oRight, nLeft)
		}
		if c > 0 {
			break
		}
		scale = nScale
		left = nLeft
		right = nRight
	}
	mid := rankCheckMid(oLeft, oRight, rankMid(left, right))
	for mScale := mid.sig; mScale > 0; mScale-- {
		nMid := mid.setScale(mScale-1, false)
		if oLeft.cmp(nMid) >= 0 || nMid.cmp(oRight) >= 0 {
			break
		}
		mid = nMid
	}
	return mid
}

type lexoRank struct {
	bucket string
	dec    lexoDecimal
}

func parseRank(s string) (lexoRank, error) {
	bucket, dec, ok := strings.Cut(s, rankBucketSep)
	if !ok || (bucket != "0" && bucket != "1" && bucket != "2") {
		return lexoRank{}, fmt.Errorf("invalid rank %q", s)
	}
	d, err := parseDecimal(dec)
	if err != nil {
		return lexoRank{}, err
	}
	return lexoRank{bucket: bucket, dec: d}, nil
}

func (r lexoRank) String() string {
	return r.bucket + rankBucketSep + r.dec.format()
}

func (r lexoRank) genNext() lexoRank {
	if r.dec.cmp(rankMinDec) == 0 {
		return lexoRank{r.bucket, rankInitMin}
	}
	next := makeDecimal(r.dec.ceil(), 0).add(rankEight)
	if next.cmp(rankMaxDec) >= 0 {
		next = rankBetween(r.dec, rankMaxDec)
	}
	return lexoRank{r.bucket, next}
}

func (r lexoRank) genPrev() lexoRank {
	if r.dec.cmp(rankMaxDec) == 0 {
		return lexoRank{r.bucket, rankInitMax}
	}
	f := r.dec.floor()
	prev := makeDecimal(f.Sub(f, big.NewInt(8)), 0)
	if prev.mag.Sign() < 0 || prev.cmp(rankMinDec) <= 0 {
		prev = rankBetween(rankMinDec, r.dec)
	}
	return lexoRank{r.bucket, prev}
}

// makeRank ports @hcengineering/rank's makeRank: a rank after prev and
// before next, either of which may be empty.
func makeRank(prev, next string) (string, error) {
	prev, next = strings.TrimSpace(prev), strings.TrimSpace(next)
	switch {
	case prev != "" && next != "":
		p, err := parseRank(prev)
		if err != nil {
			return "", err
		}
		n, err := parseRank(next)
		if err != nil {
			return "", err
		}
		switch c := p.dec.cmp(n.dec); {
		case c == 0:
			return p.genNext().String(), nil
		case c > 0:
			return lexoRank{p.bucket, rankBetween(n.dec, p.dec)}.String(), nil
		default:
			return lexoRank{p.bucket, rankBetween(p.dec, n.dec)}.String(), nil
		}
	case prev != "":
		p, err := parseRank(prev)
		if err != nil {
			return "", err
		}
		return p.genNext().String(), nil
	case next != "":
		n, err := parseRank(next)
		if err != nil {
			return "", err
		}
		return n.genPrev().String(), nil
	default:
		return lexoRank{"0", rankBetween(rankMinDec, rankMaxDec)}.String(), nil
	}
}
