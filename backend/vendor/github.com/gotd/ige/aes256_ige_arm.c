//go:build arm && cgo && (linux || android)
// +build arm
// +build cgo
// +build linux android

// AES-256 IGE decrypt using ARMv8 Cryptography Extension on AArch32.
#include <arm_neon.h>
#include <dlfcn.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

#ifndef AT_HWCAP2
#define AT_HWCAP2 26
#endif
#ifndef HWCAP2_AES
#define HWCAP2_AES (1 << 0)
#endif

static int aes_hw_available(void) {
    typedef unsigned long (*getauxval_fn)(unsigned long);
    const char *libc_name =
#if defined(__ANDROID__)
        "libc.so";
#else
        "libc.so.6";
#endif
    void *libc = dlopen(libc_name, RTLD_LAZY | RTLD_LOCAL);
    if (libc == NULL) return 0;
    getauxval_fn getauxval = (getauxval_fn)dlsym(libc, "getauxval");
    if (getauxval == NULL) {
        dlclose(libc);
        return 0;
    }
    unsigned long hw2 = getauxval(AT_HWCAP2);
    dlclose(libc);
    return (hw2 & HWCAP2_AES) ? 1 : 0;
}

static uint8_t gf_mul(uint8_t a, uint8_t b) {
    uint8_t result = 0;
    for (int i = 0; i < 8; i++) {
        uint8_t select = (uint8_t)-(int)(b & 1);
        result ^= a & select;
        uint8_t high = a >> 7;
        a = (uint8_t)((a << 1) ^ ((uint8_t)-(int)high & 0x1b));
        b >>= 1;
    }
    return result;
}

static uint8_t rotate_left(uint8_t value, int shift) {
    return (uint8_t)((value << shift) | (value >> (8 - shift)));
}

static uint8_t aes_sbox(uint8_t value) {
    uint8_t value2 = gf_mul(value, value);
    uint8_t value4 = gf_mul(value2, value2);
    uint8_t value8 = gf_mul(value4, value4);
    uint8_t value16 = gf_mul(value8, value8);
    uint8_t value32 = gf_mul(value16, value16);
    uint8_t value64 = gf_mul(value32, value32);
    uint8_t value128 = gf_mul(value64, value64);
    uint8_t inverse = gf_mul(value128, value64);
    inverse = gf_mul(inverse, value32);
    inverse = gf_mul(inverse, value16);
    inverse = gf_mul(inverse, value8);
    inverse = gf_mul(inverse, value4);
    inverse = gf_mul(inverse, value2);
    return inverse ^ rotate_left(inverse, 1) ^ rotate_left(inverse, 2) ^
           rotate_left(inverse, 3) ^ rotate_left(inverse, 4) ^ 0x63;
}

// AES-256 key expansion to 15 encryption round keys.
static void expand_key(const uint8_t *key, uint8_t ek[15][16]) {
    uint8_t w[60][4];
    memcpy(w, key, 32);
    uint8_t rcon = 1;
    for (int i = 8; i < 60; i++) {
        uint8_t t[4];
        memcpy(t, w[i - 1], 4);
        if (i % 8 == 0) {
            uint8_t tmp = t[0];
            t[0] = t[1];
            t[1] = t[2];
            t[2] = t[3];
            t[3] = tmp;
            for (int j = 0; j < 4; j++) t[j] = aes_sbox(t[j]);
            t[0] ^= rcon;
            rcon = (uint8_t)((rcon << 1) ^ ((rcon >> 7) * 0x1b));
        } else if (i % 8 == 4) {
            for (int j = 0; j < 4; j++) t[j] = aes_sbox(t[j]);
        }
        for (int j = 0; j < 4; j++) w[i][j] = w[i - 8][j] ^ t[j];
    }
    for (int r = 0; r < 15; r++) memcpy(ek[r], w[4 * r], 16);
}

// IGE decrypt: c is the previous ciphertext and m is the previous plaintext.
// Returns 0 on success, -1 if hardware AES is unavailable, and -2 on bad input.
int aes256_ige_decrypt(const uint8_t *key, const uint8_t *iv,
                       uint8_t *dst, const uint8_t *src, size_t len) {
    if (!aes_hw_available()) return -1;
    if (len == 0 || len % 16 != 0) return -2;

    uint8_t ek[15][16];
    expand_key(key, ek);

    uint8x16_t drk[15];
    drk[0] = vld1q_u8(ek[14]);
    for (int j = 1; j < 14; j++) drk[j] = vaesimcq_u8(vld1q_u8(ek[14 - j]));
    drk[14] = vld1q_u8(ek[0]);

    uint8x16_t c = vld1q_u8(iv);
    uint8x16_t m = vld1q_u8(iv + 16);

    for (size_t o = 0; o < len; o += 16) {
        uint8x16_t ct = vld1q_u8(src + o);
        uint8x16_t s = veorq_u8(ct, m);
        s = vaesdq_u8(s, drk[0]);
        s = vaesimcq_u8(s);
        for (int i = 1; i < 13; i++) {
            s = vaesdq_u8(s, drk[i]);
            s = vaesimcq_u8(s);
        }
        s = vaesdq_u8(s, drk[13]);
        s = veorq_u8(s, drk[14]);
        s = veorq_u8(s, c);
        vst1q_u8(dst + o, s);
        m = s;
        c = ct;
    }
    return 0;
}
