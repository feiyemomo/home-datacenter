package utils

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWT signing parameters. These are populated by main.go from
// config.AppConfig.JWT at startup. Defaults are intentionally
// insecure placeholders — production deployments must override
// jwt.secret in configs/config.yaml.
var (
	JWTSecret      = "PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET"
	TokenExpireDays = 365
)

// Claims is the JWT payload: identity plus standard registered claims.
type Claims struct {
	UserID   uint `json:"user_id"`
	DeviceID uint `json:"device_id"`

	jwt.RegisteredClaims
}

// GenerateToken signs a long-lived JWT for (userID, deviceID).
// Expiry is TokenExpireDays * 24h from now.
func GenerateToken(
	userID uint,
	deviceID uint,
) (string, error) {

	now := time.Now()

	claims := Claims{
		UserID:   userID,
		DeviceID: deviceID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(now),

			ExpiresAt: jwt.NewNumericDate(
				now.Add(
					time.Duration(TokenExpireDays) * 24 * time.Hour,
				),
			),
		},
	}

	token := jwt.NewWithClaims(
		jwt.SigningMethodHS256,
		claims,
	)

	return token.SignedString(
		[]byte(JWTSecret),
	)
}

// ParseToken verifies the signature and returns the claims, or an
// error if the token is invalid / expired / malformed.
func ParseToken(
	tokenString string,
) (*Claims, error) {

	token, err := jwt.ParseWithClaims(
		tokenString,
		&Claims{},
		func(token *jwt.Token) (interface{}, error) {
			return []byte(JWTSecret), nil
		},
	)

	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, jwt.ErrTokenMalformed
	}

	return claims, nil
}
