package utils

import (
    "crypto/rand"
    "crypto/sha256"
    "encoding/hex"
)

// GenerateAccessKey
//
// 生成32字节随机密钥
// 输出长度：64位十六进制字符串
//
// 示例：
// d1c46f3f7e9f7f40f1d56e0a0fdb6a9c
// 2b4b6cf39f7bc8a0e1e63d53e1a7c4fd
func GenerateAccessKey() (string, error) {
    b := make([]byte, 32)

    _, err := rand.Read(b)
    if err != nil {
        return "", err
    }

    return hex.EncodeToString(b), nil
}

// HashAccessKey
//
// 对AccessKey进行SHA256哈希
//
// 数据库只保存Hash
// 不保存明文Key
func HashAccessKey(key string) string {
    hash := sha256.Sum256([]byte(key))
    return hex.EncodeToString(hash[:])
}

// VerifyAccessKey
//
// 校验AccessKey是否匹配Hash
func VerifyAccessKey(accessKey string, storedHash string) bool {
    return HashAccessKey(accessKey) == storedHash
}