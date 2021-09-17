//go:generate go run internal/cmd/genheader/main.go

// Package jws implements the digital signature on JSON based data
// structures as described in https://tools.ietf.org/html/rfc7515
//
// If you do not care about the details, the only things that you
// would need to use are the following functions:
//
//     jws.Sign(payload, algorithm, key)
//     jws.Verify(encodedjws, algorithm, key)
//
// To sign, simply use `jws.Sign`. `payload` is a []byte buffer that
// contains whatever data you want to sign. `alg` is one of the
// jwa.SignatureAlgorithm constants from package jwa. For RSA and
// ECDSA family of algorithms, you will need to prepare a private key.
// For HMAC family, you just need a []byte value. The `jws.Sign`
// function will return the encoded JWS message on success.
//
// To verify, use `jws.Verify`. It will parse the `encodedjws` buffer
// and verify the result using `algorithm` and `key`. Upon successful
// verification, the original payload is returned, so you can work on it.
package jws

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"io"
	"io/ioutil"
	"reflect"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/lestrrat-go/jwx/internal/base64"
	"github.com/lestrrat-go/jwx/internal/json"
	"github.com/lestrrat-go/jwx/internal/pool"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jwk"
	"github.com/lestrrat-go/jwx/x25519"
	"github.com/pkg/errors"
)

var registry = json.NewRegistry()

type payloadSigner struct {
	signer    Signer
	key       interface{}
	protected Headers
	public    Headers
}

func (s *payloadSigner) Sign(payload []byte) ([]byte, error) {
	return s.signer.Sign(payload, s.key)
}

func (s *payloadSigner) Algorithm() jwa.SignatureAlgorithm {
	return s.signer.Algorithm()
}

func (s *payloadSigner) ProtectedHeader() Headers {
	return s.protected
}

func (s *payloadSigner) PublicHeader() Headers {
	return s.public
}

var signers = make(map[jwa.SignatureAlgorithm]Signer)
var muSigner = &sync.Mutex{}

// Sign generates a signature for the given payload, and serializes
// it in compact serialization format. In this format you may NOT use
// multiple signers.
//
// The `alg` parameter is the identifier for the signature algorithm
// that should be used.
//
// For the `key` parameter, any of the following is accepted:
// * A "raw" key (e.g. rsa.PrivateKey, ecdsa.PrivateKey, etc)
// * A crypto.Signer
// * A jwk.Key
//
// A `crypto.Signer` is used when the private part of a key is
// kept in an inaccessible location, such as hardware.
// `crypto.Signer` is currently supported for RSA, ECDSA, and EdDSA
// family of algorithms.
//
// If the key is a jwk.Key and the key contains a key ID (`kid` field),
// then it is added to the protected header generated by the signature
//
// The algorithm specified in the `alg` parameter must be able to support
// the type of key you provided, otherwise an error is returned.
//
// If you would like to pass custom headers, use the WithHeaders option.
//
// If the headers contain "b64" field, then the boolean value for the field
// is respected when creating the compact serialization form. That is,
// if you specify a header with `{"b64": false}`, then the payload is
// not base64 encoded.
func Sign(payload []byte, alg jwa.SignatureAlgorithm, key interface{}, options ...SignOption) ([]byte, error) {
	var hdrs Headers
	for _, o := range options {
		//nolint:forcetypeassert
		switch o.Ident() {
		case identHeaders{}:
			hdrs = o.Value().(Headers)
		}
	}

	muSigner.Lock()
	signer, ok := signers[alg]
	if !ok {
		v, err := NewSigner(alg)
		if err != nil {
			muSigner.Unlock()
			return nil, errors.Wrap(err, `failed to create signer`)
		}
		signers[alg] = v
		signer = v
	}
	muSigner.Unlock()

	sig := &Signature{protected: hdrs}
	_, signature, err := sig.Sign(payload, signer, key)
	if err != nil {
		return nil, errors.Wrap(err, `failed sign payload`)
	}

	return signature, nil
}

// SignMulti accepts multiple signers via the options parameter,
// and creates a JWS in JSON serialization format that contains
// signatures from applying aforementioned signers.
//
// Use `jws.WithSigner(...)` to specify values how to generate
// each signature in the `"signatures": [ ... ]` field.
func SignMulti(payload []byte, options ...Option) ([]byte, error) {
	var signers []*payloadSigner
	for _, o := range options {
		switch o.Ident() {
		case identPayloadSigner{}:
			signers = append(signers, o.Value().(*payloadSigner))
		}
	}

	if len(signers) == 0 {
		return nil, errors.New(`no signers provided`)
	}

	var result Message

	result.payload = payload

	result.signatures = make([]*Signature, 0, len(signers))
	for i, signer := range signers {
		protected := signer.ProtectedHeader()
		if protected == nil {
			protected = NewHeaders()
		}

		if err := protected.Set(AlgorithmKey, signer.Algorithm()); err != nil {
			return nil, errors.Wrap(err, `failed to set header`)
		}

		sig := &Signature{
			headers:   signer.PublicHeader(),
			protected: protected,
		}
		_, _, err := sig.Sign(payload, signer.signer, signer.key)
		if err != nil {
			return nil, errors.Wrapf(err, `failed to generate signature for signer #%d (alg=%s)`, i, signer.Algorithm())
		}

		result.signatures = append(result.signatures, sig)
	}

	return json.Marshal(result)
}

// Verify checks if the given JWS message is verifiable using `alg` and `key`.
// `key` may be a "raw" key (e.g. rsa.PublicKey) or a jwk.Key
//
// If the verification is successful, `err` is nil, and the content of the
// payload that was signed is returned. If you need more fine-grained
// control of the verification process, manually generate a
// `Verifier` in `verify` subpackage, and call `Verify` method on it.
// If you need to access signatures and JOSE headers in a JWS message,
// use `Parse` function to get `Message` object.
func Verify(buf []byte, alg jwa.SignatureAlgorithm, key interface{}, options ...VerifyOption) ([]byte, error) {
	var dst *Message
	var detachedPayload []byte
	//nolint:forcetypeassert
	for _, option := range options {
		switch option.Ident() {
		case identMessage{}:
			dst = option.Value().(*Message)
		case identDetachedPayload{}:
			detachedPayload = option.Value().([]byte)
		}
	}

	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		return nil, errors.New(`attempt to verify empty buffer`)
	}

	if buf[0] == '{' {
		return verifyJSON(buf, alg, key, dst, detachedPayload)
	}
	return verifyCompact(buf, alg, key, dst, detachedPayload)
}

// VerifySet uses keys store in a jwk.Set to verify the payload in `buf`.
//
// In order for `VerifySet()` to use a key in the given set, the
// `jwk.Key` object must have a valid "alg" field, and it also must
// have either an empty value or the value "sig" in the "use" field.
//
// Furthermore if the JWS signature asks for a spefici "kid", the
// `jwk.Key` must have the same "kid" as the signature.
func VerifySet(buf []byte, set jwk.Set) ([]byte, error) {
	n := set.Len()
	for i := 0; i < n; i++ {
		key, ok := set.Get(i)
		if !ok {
			continue
		}
		if key.Algorithm() == "" { // algorithm is not
			continue
		}

		if usage := key.KeyUsage(); usage != "" && usage != jwk.ForSignature.String() {
			continue
		}

		buf, err := Verify(buf, jwa.SignatureAlgorithm(key.Algorithm()), key)
		if err != nil {
			continue
		}

		return buf, nil
	}

	return nil, errors.New(`failed to verify message with any of the keys in the jwk.Set object`)
}

func verifyJSON(signed []byte, alg jwa.SignatureAlgorithm, key interface{}, dst *Message, detachedPayload []byte) ([]byte, error) {
	verifier, err := NewVerifier(alg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create verifier")
	}

	var m Message
	if err := json.Unmarshal(signed, &m); err != nil {
		return nil, errors.Wrap(err, `failed to unmarshal JSON message`)
	}

	if len(m.payload) != 0 && detachedPayload != nil {
		return nil, errors.New(`can't specify detached payload for JWS with payload`)
	}

	if detachedPayload != nil {
		m.payload = detachedPayload
	}

	// Pre-compute the base64 encoded version of payload
	var payload string
	if m.b64 {
		payload = base64.EncodeToString(m.payload)
	} else {
		payload = string(m.payload)
	}

	buf := pool.GetBytesBuffer()
	defer pool.ReleaseBytesBuffer(buf)

	for i, sig := range m.signatures {
		buf.Reset()
		if hdr := sig.headers; hdr != nil && hdr.KeyID() != "" {
			if jwkKey, ok := key.(jwk.Key); ok {
				if jwkKey.KeyID() != hdr.KeyID() {
					continue
				}
			}
		}

		protected, err := json.Marshal(sig.protected)
		if err != nil {
			return nil, errors.Wrapf(err, `failed to marshal "protected" for signature #%d`, i+1)
		}

		buf.WriteString(base64.EncodeToString(protected))
		buf.WriteByte('.')
		buf.WriteString(payload)

		if err := verifier.Verify(buf.Bytes(), sig.signature, key); err == nil {
			if dst != nil {
				*dst = m
			}
			return m.payload, nil
		}
	}
	return nil, errors.New(`could not verify with any of the signatures`)
}

// get the value of b64 header field.
// If the field does not exist, returns true (default)
// Otherwise return the value specified by the header field.
func getB64Value(hdr Headers) bool {
	b64raw, ok := hdr.Get("b64")
	if !ok {
		return true // default
	}

	b64, ok := b64raw.(bool) // default
	if !ok {
		return false
	}
	return b64
}

func verifyCompact(signed []byte, alg jwa.SignatureAlgorithm, key interface{}, dst *Message, detachedPayload []byte) ([]byte, error) {
	protected, payload, signature, err := SplitCompact(signed)
	if err != nil {
		return nil, errors.Wrap(err, `failed extract from compact serialization format`)
	}

	verifier, err := NewVerifier(alg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create verifier")
	}

	verifyBuf := pool.GetBytesBuffer()
	defer pool.ReleaseBytesBuffer(verifyBuf)

	verifyBuf.Write(protected)
	verifyBuf.WriteByte('.')
	if len(payload) == 0 && detachedPayload != nil {
		payload = detachedPayload
	}
	verifyBuf.Write(payload)

	decodedSignature, err := base64.Decode(signature)
	if err != nil {
		return nil, errors.Wrap(err, `failed to decode signature`)
	}

	hdr := NewHeaders()
	decodedProtected, err := base64.Decode(protected)
	if err != nil {
		return nil, errors.Wrap(err, `failed to decode headers`)
	}

	if err := json.Unmarshal(decodedProtected, hdr); err != nil {
		return nil, errors.Wrap(err, `failed to decode headers`)
	}

	if hdr.KeyID() != "" {
		if jwkKey, ok := key.(jwk.Key); ok {
			if jwkKey.KeyID() != hdr.KeyID() {
				return nil, errors.New(`"kid" fields do not match`)
			}
		}
	}

	if err := verifier.Verify(verifyBuf.Bytes(), decodedSignature, key); err != nil {
		return nil, errors.Wrap(err, `failed to verify message`)
	}

	var decodedPayload []byte
	if !getB64Value(hdr) { // it's not base64 encode
		decodedPayload = payload
	}

	if decodedPayload == nil {
		v, err := base64.Decode(payload)
		if err != nil {
			return nil, errors.Wrap(err, `message verified, failed to decode payload`)
		}
		decodedPayload = v
	}

	if dst != nil {
		// Construct a new Message object
		m := NewMessage()
		m.SetPayload(decodedPayload)
		sig := NewSignature()
		sig.SetProtectedHeaders(hdr)
		sig.SetSignature(decodedSignature)
		m.AppendSignature(sig)

		*dst = *m
	}
	return decodedPayload, nil
}

// This is an "optimized" ioutil.ReadAll(). It will attempt to read
// all of the contents from the reader IF the reader is of a certain
// concrete type.
func readAll(rdr io.Reader) ([]byte, bool) {
	switch rdr.(type) {
	case *bytes.Reader, *bytes.Buffer, *strings.Reader:
		data, err := ioutil.ReadAll(rdr)
		if err != nil {
			return nil, false
		}
		return data, true
	default:
		return nil, false
	}
}

// Parse parses contents from the given source and creates a jws.Message
// struct. The input can be in either compact or full JSON serialization.
func Parse(src []byte) (*Message, error) {
	for i := 0; i < len(src); i++ {
		r := rune(src[i])
		if r >= utf8.RuneSelf {
			r, _ = utf8.DecodeRune(src)
		}
		if !unicode.IsSpace(r) {
			if r == '{' {
				return parseJSON(src)
			}
			return parseCompact(src)
		}
	}
	return nil, errors.New("invalid byte sequence")
}

// Parse parses contents from the given source and creates a jws.Message
// struct. The input can be in either compact or full JSON serialization.
func ParseString(src string) (*Message, error) {
	return Parse([]byte(src))
}

// Parse parses contents from the given source and creates a jws.Message
// struct. The input can be in either compact or full JSON serialization.
func ParseReader(src io.Reader) (*Message, error) {
	if data, ok := readAll(src); ok {
		return Parse(data)
	}

	rdr := bufio.NewReader(src)
	var first rune
	for {
		r, _, err := rdr.ReadRune()
		if err != nil {
			return nil, errors.Wrap(err, `failed to read rune`)
		}
		if !unicode.IsSpace(r) {
			first = r
			if err := rdr.UnreadRune(); err != nil {
				return nil, errors.Wrap(err, `failed to unread rune`)
			}

			break
		}
	}

	var parser func(io.Reader) (*Message, error)
	if first == '{' {
		parser = parseJSONReader
	} else {
		parser = parseCompactReader
	}

	m, err := parser(rdr)
	if err != nil {
		return nil, errors.Wrap(err, `failed to parse jws message`)
	}

	return m, nil
}

func parseJSONReader(src io.Reader) (result *Message, err error) {
	var m Message
	if err := json.NewDecoder(src).Decode(&m); err != nil {
		return nil, errors.Wrap(err, `failed to unmarshal jws message`)
	}
	return &m, nil
}

func parseJSON(data []byte) (result *Message, err error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, errors.Wrap(err, `failed to unmarshal jws message`)
	}
	return &m, nil
}

// SplitCompact splits a JWT and returns its three parts
// separately: protected headers, payload and signature.
func SplitCompact(src []byte) ([]byte, []byte, []byte, error) {
	parts := bytes.Split(src, []byte("."))
	if len(parts) < 3 {
		return nil, nil, nil, errors.New(`invalid number of segments`)
	}
	return parts[0], parts[1], parts[2], nil
}

// SplitCompactString splits a JWT and returns its three parts
// separately: protected headers, payload and signature.
func SplitCompactString(src string) ([]byte, []byte, []byte, error) {
	parts := strings.Split(src, ".")
	if len(parts) < 3 {
		return nil, nil, nil, errors.New(`invalid number of segments`)
	}
	return []byte(parts[0]), []byte(parts[1]), []byte(parts[2]), nil
}

// SplitCompactReader splits a JWT and returns its three parts
// separately: protected headers, payload and signature.
func SplitCompactReader(rdr io.Reader) ([]byte, []byte, []byte, error) {
	if data, ok := readAll(rdr); ok {
		return SplitCompact(data)
	}

	var protected []byte
	var payload []byte
	var signature []byte
	var periods int
	var state int

	buf := make([]byte, 4096)
	var sofar []byte

	for {
		// read next bytes
		n, err := rdr.Read(buf)
		// return on unexpected read error
		if err != nil && err != io.EOF {
			return nil, nil, nil, errors.Wrap(err, `unexpected end of input`)
		}

		// append to current buffer
		sofar = append(sofar, buf[:n]...)
		// loop to capture multiple '.' in current buffer
		for loop := true; loop; {
			var i = bytes.IndexByte(sofar, '.')
			if i == -1 && err != io.EOF {
				// no '.' found -> exit and read next bytes (outer loop)
				loop = false
				continue
			} else if i == -1 && err == io.EOF {
				// no '.' found -> process rest and exit
				i = len(sofar)
				loop = false
			} else {
				// '.' found
				periods++
			}

			// Reaching this point means we have found a '.' or EOF and process the rest of the buffer
			switch state {
			case 0:
				protected = sofar[:i]
				state++
			case 1:
				payload = sofar[:i]
				state++
			case 2:
				signature = sofar[:i]
			}
			// Shorten current buffer
			if len(sofar) > i {
				sofar = sofar[i+1:]
			}
		}
		// Exit on EOF
		if err == io.EOF {
			break
		}
	}
	if periods != 2 {
		return nil, nil, nil, errors.New(`invalid number of segments`)
	}

	return protected, payload, signature, nil
}

// parseCompactReader parses a JWS value serialized via compact serialization.
func parseCompactReader(rdr io.Reader) (m *Message, err error) {
	protected, payload, signature, err := SplitCompactReader(rdr)
	if err != nil {
		return nil, errors.Wrap(err, `invalid compact serialization format`)
	}
	return parse(protected, payload, signature)
}

func parseCompact(data []byte) (m *Message, err error) {
	protected, payload, signature, err := SplitCompact(data)
	if err != nil {
		return nil, errors.Wrap(err, `invalid compact serialization format`)
	}
	return parse(protected, payload, signature)
}

func parse(protected, payload, signature []byte) (*Message, error) {
	decodedHeader, err := base64.Decode(protected)
	if err != nil {
		return nil, errors.Wrap(err, `failed to decode protected headers`)
	}

	hdr := NewHeaders()
	if err := json.Unmarshal(decodedHeader, hdr); err != nil {
		return nil, errors.Wrap(err, `failed to parse JOSE headers`)
	}

	decodedPayload, err := base64.Decode(payload)
	if err != nil {
		return nil, errors.Wrap(err, `failed to decode payload`)
	}

	decodedSignature, err := base64.Decode(signature)
	if err != nil {
		return nil, errors.Wrap(err, `failed to decode signature`)
	}

	var msg Message
	msg.payload = decodedPayload
	msg.signatures = append(msg.signatures, &Signature{
		protected: hdr,
		signature: decodedSignature,
	})
	return &msg, nil
}

// RegisterCustomField allows users to specify that a private field
// be decoded as an instance of the specified type. This option has
// a global effect.
//
// For example, suppose you have a custom field `x-birthday`, which
// you want to represent as a string formatted in RFC3339 in JSON,
// but want it back as `time.Time`.
//
// In that case you would register a custom field as follows
//
//   jwe.RegisterCustomField(`x-birthday`, timeT)
//
// Then `hdr.Get("x-birthday")` will still return an `interface{}`,
// but you can convert its type to `time.Time`
//
//   bdayif, _ := hdr.Get(`x-birthday`)
//   bday := bdayif.(time.Time)
//
func RegisterCustomField(name string, object interface{}) {
	registry.Register(name, object)
}

// Helpers for signature verification
var rawKeyToKeyType = make(map[reflect.Type]jwa.KeyType)
var keyTypeToAlgorithms = make(map[jwa.KeyType][]jwa.SignatureAlgorithm)

func init() {
	rawKeyToKeyType[reflect.TypeOf([]byte(nil))] = jwa.OctetSeq
	rawKeyToKeyType[reflect.TypeOf(ed25519.PublicKey(nil))] = jwa.OKP
	rawKeyToKeyType[reflect.TypeOf(rsa.PublicKey{})] = jwa.RSA
	rawKeyToKeyType[reflect.TypeOf((*rsa.PublicKey)(nil))] = jwa.RSA
	rawKeyToKeyType[reflect.TypeOf(ecdsa.PublicKey{})] = jwa.EC
	rawKeyToKeyType[reflect.TypeOf((*ecdsa.PublicKey)(nil))] = jwa.EC

	addAlgorithmForKeyType(jwa.OKP, jwa.EdDSA)
	for _, alg := range []jwa.SignatureAlgorithm{jwa.HS256, jwa.HS384, jwa.HS512} {
		addAlgorithmForKeyType(jwa.OctetSeq, alg)
	}
	for _, alg := range []jwa.SignatureAlgorithm{jwa.PS256, jwa.PS384, jwa.PS512, jwa.RS256, jwa.RS384, jwa.RS512} {
		addAlgorithmForKeyType(jwa.RSA, alg)
	}
	for _, alg := range []jwa.SignatureAlgorithm{jwa.ES256, jwa.ES384, jwa.ES512} {
		addAlgorithmForKeyType(jwa.EC, alg)
	}
}

func addAlgorithmForKeyType(kty jwa.KeyType, alg jwa.SignatureAlgorithm) {
	keyTypeToAlgorithms[kty] = append(keyTypeToAlgorithms[kty], alg)
}

// AlgorithmsForKey returns the possible signature algorithms that can
// be used for a given key
func AlgorithmsForKey(key interface{}) ([]jwa.SignatureAlgorithm, error) {
	var kty jwa.KeyType
	switch key := key.(type) {
	case jwk.Key:
		kty = key.KeyType()
	case rsa.PublicKey, *rsa.PublicKey, rsa.PrivateKey, *rsa.PrivateKey:
		kty = jwa.RSA
	case ecdsa.PublicKey, *ecdsa.PublicKey, ecdsa.PrivateKey, *ecdsa.PrivateKey:
		kty = jwa.EC
	case ed25519.PublicKey, ed25519.PrivateKey, x25519.PublicKey, x25519.PrivateKey:
		kty = jwa.OKP
	case []byte:
		kty = jwa.OctetSeq
	default:
		return nil, errors.Errorf(`invalid key %T`, key)
	}

	algs, ok := keyTypeToAlgorithms[kty]
	if !ok {
		return nil, errors.Errorf(`invalid key type %q`, kty)
	}
	return algs, nil
}
