// Package jsonvalue implements the private JSON value semantics used by
// AgentSlot identity and admission checks. It is internal so representation
// normalization does not become a public component contract.
package jsonvalue

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"sort"
)

type kind uint8

const (
	kindNull kind = iota
	kindBoolean
	kindNumber
	kindString
	kindArray
	kindObject
)

type value struct {
	kind    kind
	boolean bool
	number  number
	text    string
	array   []value
	object  map[string]value
}

type number struct {
	negative bool
	digits   string
	exponent big.Int
}

// Valid reports whether raw contains exactly one unambiguous JSON value.
// Duplicate object member names are rejected because they have no stable
// cross-decoder value semantics.
func Valid(raw []byte) bool {
	_, err := parse(raw)
	return err == nil
}

// Equal reports whether left and right are valid, unambiguous JSON values with
// the same data-model value. Object order, insignificant whitespace, equivalent
// string escapes, and mathematically equal number spellings do not differ.
func Equal(left, right []byte) bool {
	leftValue, err := parse(left)
	if err != nil {
		return false
	}
	rightValue, err := parse(right)
	if err != nil {
		return false
	}
	return equal(leftValue, rightValue)
}

// Canonical returns one deterministic JSON encoding of an unambiguous value.
// It preserves JSON data-model semantics rather than the caller's whitespace,
// object-member order, string escapes, or mathematically equivalent number
// spelling. The result is intended for private identity and digest inputs.
func Canonical(raw []byte) ([]byte, error) {
	parsed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	var result bytes.Buffer
	if err := writeCanonical(&result, parsed); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

func writeCanonical(destination *bytes.Buffer, current value) error {
	switch current.kind {
	case kindNull:
		destination.WriteString("null")
	case kindBoolean:
		if current.boolean {
			destination.WriteString("true")
		} else {
			destination.WriteString("false")
		}
	case kindNumber:
		if current.number.negative {
			destination.WriteByte('-')
		}
		destination.WriteString(current.number.digits)
		if current.number.exponent.Sign() != 0 {
			destination.WriteByte('e')
			destination.WriteString(current.number.exponent.String())
		}
	case kindString:
		encoded, err := json.Marshal(current.text)
		if err != nil {
			return err
		}
		destination.Write(encoded)
	case kindArray:
		destination.WriteByte('[')
		for index, item := range current.array {
			if index > 0 {
				destination.WriteByte(',')
			}
			if err := writeCanonical(destination, item); err != nil {
				return err
			}
		}
		destination.WriteByte(']')
	case kindObject:
		names := make([]string, 0, len(current.object))
		for name := range current.object {
			names = append(names, name)
		}
		sort.Strings(names)
		destination.WriteByte('{')
		for index, name := range names {
			if index > 0 {
				destination.WriteByte(',')
			}
			encoded, err := json.Marshal(name)
			if err != nil {
				return err
			}
			destination.Write(encoded)
			destination.WriteByte(':')
			if err := writeCanonical(destination, current.object[name]); err != nil {
				return err
			}
		}
		destination.WriteByte('}')
	default:
		return errors.New("jsonvalue: unsupported canonical value")
	}
	return nil
}

func parse(raw []byte) (value, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	parsed, err := decode(decoder)
	if err != nil {
		return value{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return value{}, errors.New("jsonvalue: multiple top-level values")
		}
		return value{}, err
	}
	return parsed, nil
}

func decode(decoder *json.Decoder) (value, error) {
	token, err := decoder.Token()
	if err != nil {
		return value{}, err
	}
	switch token := token.(type) {
	case nil:
		return value{kind: kindNull}, nil
	case bool:
		return value{kind: kindBoolean, boolean: token}, nil
	case string:
		return value{kind: kindString, text: token}, nil
	case json.Number:
		normalized, err := normalizeNumber(token.String())
		if err != nil {
			return value{}, err
		}
		return value{kind: kindNumber, number: normalized}, nil
	case json.Delim:
		switch token {
		case '[':
			items := make([]value, 0)
			for decoder.More() {
				item, err := decode(decoder)
				if err != nil {
					return value{}, err
				}
				items = append(items, item)
			}
			if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
				if err != nil {
					return value{}, err
				}
				return value{}, errors.New("jsonvalue: invalid array closing delimiter")
			}
			return value{kind: kindArray, array: items}, nil
		case '{':
			members := make(map[string]value)
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return value{}, err
				}
				name, ok := nameToken.(string)
				if !ok {
					return value{}, errors.New("jsonvalue: object member name is not a string")
				}
				if _, duplicate := members[name]; duplicate {
					return value{}, errors.New("jsonvalue: duplicate object member")
				}
				member, err := decode(decoder)
				if err != nil {
					return value{}, err
				}
				members[name] = member
			}
			if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
				if err != nil {
					return value{}, err
				}
				return value{}, errors.New("jsonvalue: invalid object closing delimiter")
			}
			return value{kind: kindObject, object: members}, nil
		default:
			return value{}, errors.New("jsonvalue: unexpected delimiter")
		}
	default:
		return value{}, errors.New("jsonvalue: unsupported token")
	}
}

func equal(left, right value) bool {
	if left.kind != right.kind {
		return false
	}
	switch left.kind {
	case kindNull:
		return true
	case kindBoolean:
		return left.boolean == right.boolean
	case kindNumber:
		return left.number.negative == right.number.negative &&
			left.number.digits == right.number.digits &&
			left.number.exponent.Cmp(&right.number.exponent) == 0
	case kindString:
		return left.text == right.text
	case kindArray:
		if len(left.array) != len(right.array) {
			return false
		}
		for index := range left.array {
			if !equal(left.array[index], right.array[index]) {
				return false
			}
		}
		return true
	case kindObject:
		if len(left.object) != len(right.object) {
			return false
		}
		for name, leftMember := range left.object {
			rightMember, ok := right.object[name]
			if !ok || !equal(leftMember, rightMember) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func normalizeNumber(raw string) (number, error) {
	negative := false
	if len(raw) > 0 && raw[0] == '-' {
		negative = true
		raw = raw[1:]
	}
	exponentText := "0"
	if index := bytes.IndexAny([]byte(raw), "eE"); index >= 0 {
		exponentText = raw[index+1:]
		raw = raw[:index]
	}
	if len(exponentText) > 0 && exponentText[0] == '+' {
		exponentText = exponentText[1:]
	}
	var exponent big.Int
	if _, ok := exponent.SetString(exponentText, 10); !ok {
		return number{}, errors.New("jsonvalue: invalid number exponent")
	}
	fractionDigits := 0
	if point := bytes.IndexByte([]byte(raw), '.'); point >= 0 {
		fractionDigits = len(raw) - point - 1
		raw = raw[:point] + raw[point+1:]
	}
	digits := bytes.TrimLeft([]byte(raw), "0")
	if len(digits) == 0 {
		return number{digits: "0"}, nil
	}
	trailingZeros := 0
	for len(digits) > 0 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		trailingZeros++
	}
	exponent.Sub(&exponent, big.NewInt(int64(fractionDigits)))
	exponent.Add(&exponent, big.NewInt(int64(trailingZeros)))
	return number{negative: negative, digits: string(digits), exponent: exponent}, nil
}
