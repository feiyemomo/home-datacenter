package utils

import (
    "time"

    "github.com/golang-jwt/jwt/v5"
)

const (
    // 后续改为配置文件读取
    JWTSecret = "PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET"

    // Token有效期（365天）
    TokenExpireDays = 365
)

type Claims struct {
    UserID   uint `json:"user_id"`
    DeviceID uint `json:"device_id"`

    jwt.RegisteredClaims
}

// GenerateToken 生成JWT
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
                    TokenExpireDays * 24 * time.Hour,
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

// ParseToken 解析JWT
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