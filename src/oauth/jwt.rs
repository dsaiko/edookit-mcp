//! HS256 JWT issue/verify + PKCE S256 verification. Port of Go's
//! `internal/oauth/jwt.go`. Symmetric keys are fine: the same process issues
//! and validates, so there's no separate verifier needing public-key crypto.

use std::sync::LazyLock;

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD as B64URL;
use hmac::{Hmac, KeyInit, Mac};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;

type HmacSha256 = Hmac<Sha256>;

/// Fixed HS256 header; its base64url form is precomputed once.
const JWT_HEADER: &str = r#"{"alg":"HS256","typ":"JWT"}"#;
static JWT_HEADER_B64: LazyLock<String> = LazyLock::new(|| B64URL.encode(JWT_HEADER));

/// Access-token payload: standard registered claims + a free-form `scope`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Claims {
    pub iss: String,
    pub sub: String,
    pub aud: String,
    pub iat: i64,
    pub exp: i64,
    pub jti: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub scope: String,
}

#[derive(Debug, PartialEq, Eq)]
pub enum JwtError {
    Malformed,
    BadSignature,
    DecodePayload,
    ParsePayload,
    IssuerMismatch,
    AudienceMismatch,
    Expired,
    IssuedInFuture,
}

impl JwtError {
    /// Short token for the WWW-Authenticate error_description (not for debug logs).
    pub fn as_str(&self) -> &'static str {
        match self {
            JwtError::Malformed => "malformed JWT",
            JwtError::BadSignature => "bad signature",
            JwtError::DecodePayload => "decode payload",
            JwtError::ParsePayload => "parse payload",
            JwtError::IssuerMismatch => "issuer mismatch",
            JwtError::AudienceMismatch => "audience mismatch",
            JwtError::Expired => "expired",
            JwtError::IssuedInFuture => "issued in the future",
        }
    }
}

/// Signs `claims` with the HS256 secret, returning the compact JWT.
pub fn sign(secret: &[u8], claims: &Claims) -> Result<String, String> {
    let payload = serde_json::to_vec(claims).map_err(|e| format!("marshal claims: {e}"))?;
    let payload_b64 = B64URL.encode(payload);
    let signing = format!("{}.{}", *JWT_HEADER_B64, payload_b64);
    let sig_b64 = B64URL.encode(hmac_sha256(secret, signing.as_bytes()));
    Ok(format!("{signing}.{sig_b64}"))
}

/// Parses, signature-verifies and claim-checks a token issued by this server.
/// Returns the validated claims. `now` is unix seconds.
pub fn verify(
    secret: &[u8],
    token: &str,
    iss: &str,
    aud: &str,
    now: i64,
) -> Result<Claims, JwtError> {
    let parts: Vec<&str> = token.split('.').collect();
    if parts.len() != 3 {
        return Err(JwtError::Malformed);
    }
    let (header_b64, payload_b64, sig_b64) = (parts[0], parts[1], parts[2]);

    // Constant-time signature compare.
    let signing = format!("{header_b64}.{payload_b64}");
    let expected = hmac_sha256(secret, signing.as_bytes());
    let provided = B64URL.decode(sig_b64).map_err(|_| JwtError::BadSignature)?;
    if expected.as_slice().ct_eq(provided.as_slice()).unwrap_u8() != 1 {
        return Err(JwtError::BadSignature);
    }

    let payload = B64URL
        .decode(payload_b64)
        .map_err(|_| JwtError::DecodePayload)?;
    let claims: Claims = serde_json::from_slice(&payload).map_err(|_| JwtError::ParsePayload)?;

    if claims.iss != iss {
        return Err(JwtError::IssuerMismatch);
    }
    if claims.aud != aud {
        return Err(JwtError::AudienceMismatch);
    }
    // RFC 7519 §4.1.4: a token is expired when now >= exp.
    if claims.exp <= now {
        return Err(JwtError::Expired);
    }
    // 5s leeway on iat for clock drift (zero here by construction, but explicit).
    if claims.iat > now + 5 {
        return Err(JwtError::IssuedInFuture);
    }
    Ok(claims)
}

/// Confirms base64url(SHA-256(verifier)) == challenge. Method must be "S256".
pub fn pkce_verify(verifier: &str, challenge: &str, method: &str) -> Result<(), String> {
    if method != "S256" {
        return Err(format!("unsupported code_challenge_method: {method}"));
    }
    let got = B64URL.encode(Sha256::digest(verifier.as_bytes()));
    if got.as_bytes().ct_eq(challenge.as_bytes()).unwrap_u8() != 1 {
        return Err("PKCE verifier does not match challenge".to_string());
    }
    Ok(())
}

fn hmac_sha256(secret: &[u8], data: &[u8]) -> Vec<u8> {
    let mut mac = HmacSha256::new_from_slice(secret).expect("HMAC accepts any key length");
    mac.update(data);
    mac.finalize().into_bytes().to_vec()
}

#[cfg(test)]
mod tests {
    use super::*;

    const SECRET: &[u8] = b"test-secret-at-least-32-bytes-long!!";

    fn claims(now: i64) -> Claims {
        Claims {
            iss: "https://mcp.example".into(),
            sub: "dusan".into(),
            aud: "https://mcp.example/mcp".into(),
            iat: now,
            exp: now + 3600,
            jti: "jti1".into(),
            scope: "openid offline_access".into(),
        }
    }

    #[test]
    fn sign_verify_roundtrip() {
        let tok = sign(SECRET, &claims(1000)).unwrap();
        let got = verify(
            SECRET,
            &tok,
            "https://mcp.example",
            "https://mcp.example/mcp",
            2000,
        )
        .unwrap();
        assert_eq!(got.sub, "dusan");
        assert_eq!(got.scope, "openid offline_access");
    }

    #[test]
    fn tampered_signature_rejected() {
        let mut tok = sign(SECRET, &claims(1000)).unwrap();
        tok.pop();
        tok.push(if tok.ends_with('A') { 'B' } else { 'A' });
        assert_eq!(
            verify(
                SECRET,
                &tok,
                "https://mcp.example",
                "https://mcp.example/mcp",
                2000
            ),
            Err(JwtError::BadSignature)
        );
    }

    #[test]
    fn wrong_secret_rejected() {
        let tok = sign(SECRET, &claims(1000)).unwrap();
        assert_eq!(
            verify(
                b"another-secret-also-32-bytes-long!!!",
                &tok,
                "https://mcp.example",
                "https://mcp.example/mcp",
                2000
            ),
            Err(JwtError::BadSignature)
        );
    }

    #[test]
    fn audience_and_issuer_checked() {
        let tok = sign(SECRET, &claims(1000)).unwrap();
        assert_eq!(
            verify(
                SECRET,
                &tok,
                "https://mcp.example",
                "https://wrong/mcp",
                2000
            ),
            Err(JwtError::AudienceMismatch)
        );
        assert_eq!(
            verify(
                SECRET,
                &tok,
                "https://wrong",
                "https://mcp.example/mcp",
                2000
            ),
            Err(JwtError::IssuerMismatch)
        );
    }

    #[test]
    fn expiry_and_future_checked() {
        let tok = sign(SECRET, &claims(1000)).unwrap();
        // exp == now → expired.
        assert_eq!(
            verify(
                SECRET,
                &tok,
                "https://mcp.example",
                "https://mcp.example/mcp",
                4600
            ),
            Err(JwtError::Expired)
        );
        // iat far in the future.
        let future = sign(SECRET, &claims(10_000)).unwrap();
        assert_eq!(
            verify(
                SECRET,
                &future,
                "https://mcp.example",
                "https://mcp.example/mcp",
                100
            ),
            Err(JwtError::IssuedInFuture)
        );
    }

    #[test]
    fn pkce_s256_roundtrip() {
        // verifier → challenge = base64url(sha256(verifier))
        let verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
        let challenge = B64URL.encode(Sha256::digest(verifier.as_bytes()));
        assert!(pkce_verify(verifier, &challenge, "S256").is_ok());
        assert!(pkce_verify("wrong-verifier", &challenge, "S256").is_err());
        assert!(pkce_verify(verifier, &challenge, "plain").is_err());
    }
}
