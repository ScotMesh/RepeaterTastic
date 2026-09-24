/*
 * i2cshim.c -- an LD_PRELOAD shim that gives a stock meshtasticd an I²C sensor
 *              that RepeaterTastic writes the readings for.
 *
 * WHY THIS EXISTS
 * ---------------
 * RepeaterTastic hosts several Meshtastic identities on one radio, each one a
 * stock, unpatched `meshtasticd`. We want one real sensor on the host to appear
 * to as many of those identities as we like, each publishing it as its own
 * sensor -- broadcasting it on its own schedule and answering directed telemetry
 * requests itself.
 *
 * The only way to do that without touching the firmware is to let the node
 * believe it owns the hardware. So we do not send telemetry packets: we fake the
 * bus. Every node gets a device path that does not exist in the kernel, this
 * object preloaded, and a values file. meshtasticd opens the path, scans it,
 * finds "chips", initialises their drivers, and reads them forever -- and every
 * byte it reads is synthesised here from the values file that RepeaterTastic
 * keeps up to date (see internal/sensors). Nothing in the firmware knows.
 *
 * WHAT IT INTERCEPTS
 * ------------------
 * open/openat (+64 variants), close, read, __read_chk, write and ioctl. An
 * open() of a path in $I2CSHIM_DEV returns a real kernel fd onto /dev/null -- so
 * poll/dup/close/fstat all behave -- which we then recognise and answer
 * ourselves. Addresses we do not emulate are NAKed with ENXIO, so the firmware's
 * 112-address scan does not invent devices. Every other fd falls straight
 * through to libc.
 *
 * TWO GOTCHAS THAT COST US A DAY EACH
 * -----------------------------------
 *  1. FORTIFY. meshtasticd is built with _FORTIFY_SOURCE, so a read() whose
 *     length glibc cannot prove safe -- exactly Portduino's
 *     `::read(i2c_file, RXbuf, count)` -- is emitted as __read_chk, not read.
 *     Interpose read() alone and the scan works while every register read comes
 *     back as zeroes. We interpose both.
 *  2. The register pointer survives a repeated START. Portduino's
 *     getRegisterValue() does write(reg) + endTransmission(true), and then
 *     requestFrom() re-issues ioctl(I2C_SLAVE) before a bare read(). A real chip
 *     keeps its register pointer across that, so we only invalidate it when the
 *     target address actually changes. Reset it on every I2C_SLAVE and every
 *     separate-path register read returns register 0.
 *
 * Both read paths must work: the combined I2C_RDWR transfer (Adafruit_BusIO's
 * write_then_read) and the separate write() + read() pair.
 *
 * ENVIRONMENT
 * -----------
 *   I2CSHIM_DEV      comma-separated device paths to fake (default /dev/i2c-fake)
 *   I2CSHIM_CHIPS    comma-separated chips to present (default pct2075):
 *                    pct2075, mcp9808, ina226, aht10, pmsa003i
 *   I2CSHIM_VALUES   path to the readings file, re-read on every access
 *   I2CSHIM_LINEBUF  1 = line-buffer stdout, so `docker logs` is prompt
 *   I2CSHIM_DEBUG    1 = trace every bus operation to stderr
 *
 * VALUES FILE
 * -----------
 * "field=value" lines, one per line; "field: value" and JSON-ish
 * `"field": value,` are accepted too. Unknown keys, junk and a missing file are
 * ignored -- a transfer is never failed over the file's contents, the chip just
 * reports its default. Field names and units are exactly those of
 * internal/sensors (api.go): temperature °C, humidity %, voltage V, current A,
 * pm10/pm25/pm100 µg/m³.
 *
 * PORTABILITY
 * -----------
 * Build inside debian:trixie (see build.sh) and keep the glibc requirement at
 * 2.34 or lower, so one object works on every meshtasticd image we support. In
 * particular do not use strtol/atoi: glibc 2.38+ redirects them to
 * __isoc23_strtol, which older images do not have. strtod is safe.
 *
 * Build: cc -shared -fPIC -O2 -o i2cshim.so i2cshim.c -ldl -lpthread -lm
 */

#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <math.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>

/* ---- kernel i2c-dev ABI (inlined so we need no kernel headers) ----------- */
#define I2C_SLAVE 0x0703
#define I2C_TENBIT 0x0704
#define I2C_FUNCS 0x0705
#define I2C_SLAVE_FORCE 0x0706
#define I2C_RDWR 0x0707
#define I2C_PEC 0x0708
#define I2C_SMBUS 0x0720
#define I2C_M_RD 0x0001

struct i2c_msg {
    uint16_t addr;
    uint16_t flags;
    uint16_t len;
    uint8_t *buf;
};
struct i2c_rdwr_ioctl_data {
    struct i2c_msg *msgs;
    uint32_t nmsgs;
};

#define I2C_SMBUS_QUICK 0
#define I2C_SMBUS_BYTE 1
#define I2C_SMBUS_BYTE_DATA 2
#define I2C_SMBUS_WORD_DATA 3
#define I2C_SMBUS_READ 1
#define I2C_SMBUS_WRITE 0

union i2c_smbus_data {
    uint8_t byte;
    uint16_t word;
    uint8_t block[34];
};
struct i2c_smbus_ioctl_data {
    uint8_t read_write;
    uint8_t command;
    uint32_t size;
    union i2c_smbus_data *data;
};

/* ---- real libc entry points --------------------------------------------- */
static int (*r_open)(const char *, int, ...);
static int (*r_open64)(const char *, int, ...);
static int (*r_openat)(int, const char *, int, ...);
static int (*r_openat64)(int, const char *, int, ...);
static int (*r_close)(int);
static int (*r_ioctl)(int, unsigned long, ...);
static ssize_t (*r_read)(int, void *, size_t);
static ssize_t (*r_read_chk)(int, void *, size_t, size_t);
static ssize_t (*r_write)(int, const void *, size_t);

/* ---- the chips we can imitate ------------------------------------------- */
/* Which chip carries which field is fixed and must match internal/sensors:
 * the firmware merges every sensor into one packet and the last writer wins, so
 * exactly one emulated chip carries each field.
 *
 *   pct2075  0x37  temperature
 *   mcp9808  0x18  temperature       (alternative to pct2075, never both)
 *   ina226   0x40  voltage, current
 *   aht10    0x38  humidity          (+ temperature, see below)
 *   pmsa003i 0x12  pm10, pm25, pm100
 */
enum chip {
    CHIP_PCT2075,
    CHIP_MCP9808,
    CHIP_INA226,
    CHIP_AHT10,
    CHIP_PMSA003I,
    CHIP_COUNT
};

static const struct {
    const char *name;
    uint8_t addr;
} g_chipdef[CHIP_COUNT] = {
    [CHIP_PCT2075] = {"pct2075", 0x37},
    [CHIP_MCP9808] = {"mcp9808", 0x18},
    [CHIP_INA226] = {"ina226", 0x40},
    [CHIP_AHT10] = {"aht10", 0x38},
    [CHIP_PMSA003I] = {"pmsa003i", 0x12},
};

/* ---- configuration ------------------------------------------------------- */
#define MAX_PATHS 8
#define MAX_SLOTS 32
#define MAX_READ 64

static char g_paths[MAX_PATHS][256];
static int g_npaths;
static const char *g_values;
static int g_debug;
static int g_on[CHIP_COUNT];

/* per-fd state: the current slave address and its register pointer */
struct slot {
    int fd;
    int addr;    /* current I2C_SLAVE target, -1 if unset */
    uint8_t reg; /* last register pointer written */
    int reg_valid;
};
static struct slot g_slots[MAX_SLOTS];
static pthread_mutex_t g_lock = PTHREAD_MUTEX_INITIALIZER;

/* Shadow of host-written registers, so a read of e.g. INA226 CALIBRATION echoes
 * back what the driver configured instead of a surprise. */
static uint16_t g_shadow[128][256];

static void trace(const char *fmt, ...)
{
    if (!g_debug || !r_write)
        return;
    char buf[512];
    va_list ap;
    va_start(ap, fmt);
    int n = vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    if (n > 0)
        r_write(2, buf, (size_t)n < sizeof(buf) ? (size_t)n : sizeof(buf) - 1);
}

/* One comma-separated token at a time. Returns the token length, 0 at the end. */
static size_t next_token(const char **p, const char **tok)
{
    const char *s = *p;
    while (*s == ',' || *s == ' ' || *s == '\t')
        s++;
    if (!*s) {
        *p = s;
        return 0;
    }
    const char *e = s;
    while (*e && *e != ',')
        e++;
    const char *te = e;
    while (te > s && (te[-1] == ' ' || te[-1] == '\t'))
        te--;
    *tok = s;
    *p = e;
    return (size_t)(te - s);
}

static void __attribute__((constructor)) shim_init(void)
{
    r_open = dlsym(RTLD_NEXT, "open");
    r_open64 = dlsym(RTLD_NEXT, "open64");
    r_openat = dlsym(RTLD_NEXT, "openat");
    r_openat64 = dlsym(RTLD_NEXT, "openat64");
    r_close = dlsym(RTLD_NEXT, "close");
    r_ioctl = dlsym(RTLD_NEXT, "ioctl");
    r_read = dlsym(RTLD_NEXT, "read");
    r_read_chk = dlsym(RTLD_NEXT, "__read_chk");
    r_write = dlsym(RTLD_NEXT, "write");

    for (int i = 0; i < MAX_SLOTS; i++)
        g_slots[i].fd = -1;

    const char *dev = getenv("I2CSHIM_DEV");
    if (!dev || !*dev)
        dev = "/dev/i2c-fake";
    const char *p = dev, *tok;
    size_t len;
    while ((len = next_token(&p, &tok)) != 0 && g_npaths < MAX_PATHS) {
        if (len < sizeof(g_paths[0])) {
            memcpy(g_paths[g_npaths], tok, len);
            g_paths[g_npaths][len] = 0;
            g_npaths++;
        }
    }

    g_values = getenv("I2CSHIM_VALUES");
    const char *dbg = getenv("I2CSHIM_DEBUG");
    g_debug = dbg && *dbg && *dbg != '0';

    const char *chips = getenv("I2CSHIM_CHIPS");
    if (!chips || !*chips)
        chips = "pct2075";
    p = chips;
    while ((len = next_token(&p, &tok)) != 0) {
        int hit = 0;
        for (int c = 0; c < CHIP_COUNT; c++)
            if (strlen(g_chipdef[c].name) == len && memcmp(g_chipdef[c].name, tok, len) == 0) {
                g_on[c] = 1;
                hit = 1;
            }
        if (!hit)
            trace("[i2cshim] unknown chip \"%.*s\" ignored\n", (int)len, tok);
    }

    /* meshtasticd's stdout is block-buffered when it is a pipe, which makes
     * `docker logs` lag by up to a block. Opt-in line buffering so log evidence
     * appears promptly. Purely a debugging aid; off by default. */
    const char *lb = getenv("I2CSHIM_LINEBUF");
    if (lb && *lb && *lb != '0')
        setvbuf(stdout, NULL, _IOLBF, 0);

    trace("[i2cshim] init: dev=%s values=%s chips=%s\n", dev, g_values ? g_values : "(none)", chips);
}

static int path_is_fake(const char *path)
{
    if (!path)
        return 0;
    for (int i = 0; i < g_npaths; i++)
        if (strcmp(path, g_paths[i]) == 0)
            return 1;
    return 0;
}

/* Which enabled chip answers for this address, or -1 for nobody. */
static int chip_at(int addr)
{
    for (int c = 0; c < CHIP_COUNT; c++)
        if (g_on[c] && g_chipdef[c].addr == addr)
            return c;
    return -1;
}

/* ---- the values file ----------------------------------------------------- */
/* Deliberately forgiving, and re-read on every access so another process can
 * change a reading with no restart. Mirrors sensors.ParseValues in Go. */
static int value_lookup(const char *key, double *out)
{
    if (!g_values || !r_read)
        return 0;
    int fd = r_open64 ? r_open64(g_values, O_RDONLY | O_CLOEXEC) : r_open(g_values, O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        return 0;
    char buf[8192];
    ssize_t n = r_read(fd, buf, sizeof(buf) - 1);
    if (r_close)
        r_close(fd);
    if (n <= 0)
        return 0;
    buf[n] = 0;

    size_t klen = strlen(key);
    int found = 0;
    char *line = buf;
    while (line && *line) {
        char *eol = strchr(line, '\n');
        if (eol)
            *eol = 0;

        /* split on the first = or : */
        char *sep = line;
        while (*sep && *sep != '=' && *sep != ':')
            sep++;
        if (!*sep) {
            line = eol ? eol + 1 : NULL;
            continue;
        }
        char *ks = line, *ke = sep, *vs = sep + 1;

        /* trim the key of whitespace, JSON punctuation and quotes */
        while (ks < ke && (*ks == ' ' || *ks == '\t' || *ks == '{' || *ks == '"' || *ks == '\''))
            ks++;
        while (ke > ks && (ke[-1] == ' ' || ke[-1] == '\t' || ke[-1] == '"' || ke[-1] == '\''))
            ke--;
        if (*ks == '#' || (size_t)(ke - ks) != klen || strncmp(ks, key, klen) != 0) {
            line = eol ? eol + 1 : NULL;
            continue;
        }

        while (*vs == ' ' || *vs == '\t' || *vs == '"' || *vs == '\'')
            vs++;
        char *end = NULL;
        double v = strtod(vs, &end);
        if (end && end != vs && isfinite(v)) {
            *out = v;
            found = 1; /* a later line wins, as in the Go parser */
        }
        line = eol ? eol + 1 : NULL;
    }
    return found;
}

static double value_or(const char *key, double dflt)
{
    double v = dflt;
    value_lookup(key, &v);
    return v;
}

static uint16_t clamp_u16(double v)
{
    if (!(v > 0.0))
        return 0;
    if (v > 65535.0)
        return 65535;
    return (uint16_t)lround(v);
}

static void put_be16(uint8_t *p, uint16_t v)
{
    p[0] = (uint8_t)(v >> 8);
    p[1] = (uint8_t)(v & 0xFF);
}

/* ---- chip register / frame models --------------------------------------- */
/*
 * Each model fills up to `len` bytes for a read of `reg`. Register-model chips
 * (pct2075, mcp9808, ina226) answer from the register pointer; the two
 * command-protocol chips key off the requested length instead, which is what
 * their drivers actually distinguish:
 *
 *   aht10    1 byte  -> status; 6 bytes -> the measurement frame
 *   pmsa003i         -> a 32-byte frame from offset 0, every time
 *
 * Returns the number of bytes produced, or -1 to NAK the transfer.
 */

static size_t model_pct2075(int reg, uint8_t *tmp)
{
    switch (reg) {
    case 0x00: { /* TEMP: 11-bit two's complement, left-aligned, 0.125 °C/LSB */
        double t = value_or("temperature", 20.0);
        if (t > 127.0)
            t = 127.0;
        if (t < -55.0)
            t = -55.0;
        int16_t raw = (int16_t)(((int)lround(t / 0.125)) << 5);
        put_be16(tmp, (uint16_t)raw);
        return 2;
    }
    case 0x01: /* CONF */
        tmp[0] = (uint8_t)g_shadow[0x37][0x01];
        return 1;
    case 0x02: /* THYST */
    case 0x03: /* TOS   */
        put_be16(tmp, g_shadow[0x37][reg]);
        return 2;
    case 0x04: /* TIDLE */
        tmp[0] = (uint8_t)(g_shadow[0x37][0x04] ? g_shadow[0x37][0x04] : 1);
        return 1;
    default:
        put_be16(tmp, 0);
        return 2;
    }
}

static size_t model_mcp9808(int reg, uint8_t *tmp)
{
    uint16_t v;
    switch (reg) {
    case 0x05: { /* AMBIENT_TEMP: 13-bit, 0.0625 °C/LSB, bit 12 is the sign */
        double t = value_or("temperature", 20.0);
        v = (uint16_t)((int)lround(fabs(t) / 0.0625) & 0x0FFF);
        if (t < 0)
            v |= 0x1000;
        break;
    }
    case 0x06:
        v = 0x0054; /* Microchip manufacturer ID */
        break;
    case 0x07:
        v = 0x0400; /* device ID + revision -- what the firmware scan checks */
        break;
    case 0x08:
        v = g_shadow[0x18][0x08] & 0x03;
        break;
    default:
        v = g_shadow[0x18][reg & 0xFF];
        break;
    }
    put_be16(tmp, v);
    return 2;
}

static size_t model_ina226(int reg, uint8_t *tmp)
{
    double volts = value_or("voltage", 4.05);
    double amps = value_or("current", 0.0); /* the values file is in amps */
    uint16_t v;
    switch (reg) {
    case 0x00: /* CONFIG */
        v = g_shadow[0x40][0x00] ? g_shadow[0x40][0x00] : 0x4127;
        break;
    case 0x01: /* SHUNT VOLTAGE, 2.5 µV/LSB, assuming a 0.1 Ω shunt */
        v = (uint16_t)(int16_t)lround(amps * 0.100 * 1e6 / 2.5);
        break;
    case 0x02: /* BUS VOLTAGE, 1.25 mV/LSB */
        v = (uint16_t)lround(volts / 0.00125);
        break;
    case 0x03: /* POWER, 25 × current_LSB per LSB */
        v = (uint16_t)lround(fabs(amps) * volts / (25.0 * 2.5e-5));
        break;
    case 0x04: /* CURRENT, current_LSB per LSB */
        v = (uint16_t)(int16_t)lround(amps / 2.5e-5);
        break;
    case 0x05: /* CALIBRATION */
        v = g_shadow[0x40][0x05] ? g_shadow[0x40][0x05] : 2048;
        break;
    case 0x06: /* MASK/ENABLE */
    case 0x07: /* ALERT LIMIT */
        v = g_shadow[0x40][reg];
        break;
    case 0xFE:
        v = 0x5449; /* 'TI' manufacturer ID -- the scan insists on this */
        break;
    case 0xFF:
        v = 0x2260; /* INA226 die ID */
        break;
    default:
        v = 0;
        break;
    }
    put_be16(tmp, v);
    return 2;
}

/* AHT10/AHT20 as Adafruit_AHTX0 drives it. begin() soft-resets (0xBA), sends
 * the calibrate command (0xE1 0x08 0x00) and then polls a one-byte status until
 * BUSY clears and CALIBRATED is set; getEvent() sends the trigger (0xAC 0x33
 * 0x00), polls status again and reads six bytes. We are always ready, so status
 * is a constant. The firmware's AHT10Sensor fills each metric only
 * `if (!has_*)`, so the humidity here is what reaches the packet -- and we
 * report the file's temperature too, so the reading is right whichever sensor
 * happens to claim temperature first. */
static size_t model_aht10(uint8_t *tmp, size_t len)
{
    tmp[0] = 0x18; /* CALIBRATED (0x08), not BUSY (0x80) */
    if (len <= 1)
        return 1;

    double hum = value_or("humidity", 50.0);
    if (hum < 0.0)
        hum = 0.0;
    if (hum > 100.0)
        hum = 100.0;
    double t = value_or("temperature", 20.0);
    if (t < -50.0)
        t = -50.0;
    if (t > 150.0)
        t = 150.0;

    uint32_t h = (uint32_t)lround(hum / 100.0 * 1048576.0) & 0xFFFFF;
    uint32_t tt = (uint32_t)lround((t + 50.0) / 200.0 * 1048576.0) & 0xFFFFF;

    tmp[1] = (uint8_t)(h >> 12);
    tmp[2] = (uint8_t)((h >> 4) & 0xFF);
    tmp[3] = (uint8_t)(((h & 0x0F) << 4) | ((tt >> 16) & 0x0F));
    tmp[4] = (uint8_t)((tt >> 8) & 0xFF);
    tmp[5] = (uint8_t)(tt & 0xFF);
    return 6;
}

/* PMSA003I as PMSA003ISensor drives it: one bare requestFrom() of
 * PMSA003I_FRAME_LENGTH bytes, no register write at all. The frame is the
 * Plantower one -- 0x42 0x4D header, a big-endian 16-bit body, and an additive
 * checksum of every byte before it. The firmware rejects the frame on a bad
 * header or checksum, so both have to be right. */
#define PMS_FRAME 32

static size_t model_pmsa003i(uint8_t *tmp, size_t len)
{
    uint8_t f[PMS_FRAME];
    memset(f, 0, sizeof(f));
    f[0] = 0x42;
    f[1] = 0x4D;
    put_be16(f + 2, PMS_FRAME - 4); /* frame length: everything after this field */

    uint16_t pm10 = clamp_u16(value_or("pm10", 0.0));   /* PM1.0, as Meshtastic names it */
    uint16_t pm25 = clamp_u16(value_or("pm25", 0.0));   /* PM2.5 */
    uint16_t pm100 = clamp_u16(value_or("pm100", 0.0)); /* PM10  */

    put_be16(f + 4, pm10); /* standard particle */
    put_be16(f + 6, pm25);
    put_be16(f + 8, pm100);
    put_be16(f + 10, pm10); /* atmospheric environment */
    put_be16(f + 12, pm25);
    put_be16(f + 14, pm100);
    /* 16..27: particle counts per 0.1 L. We have no source for these, and the
     * firmware is happy to publish zeroes. 28: version, 29: error code. */

    uint16_t sum = 0;
    for (size_t i = 0; i < PMS_FRAME - 2; i++)
        sum = (uint16_t)(sum + f[i]);
    put_be16(f + PMS_FRAME - 2, sum);

    size_t n = len < PMS_FRAME ? len : PMS_FRAME;
    memcpy(tmp, f, n);
    return n;
}

/* Fill out[0..len) for a read of `reg` from `addr`. -1 NAKs. */
static int chip_read(int addr, int reg, uint8_t *out, size_t len)
{
    int c = chip_at(addr);
    if (c < 0)
        return -1;
    if (len > MAX_READ)
        len = MAX_READ;

    uint8_t tmp[MAX_READ];
    memset(tmp, 0, sizeof(tmp));
    size_t w = 0;

    switch (c) {
    case CHIP_PCT2075:
        w = model_pct2075(reg, tmp);
        break;
    case CHIP_MCP9808:
        w = model_mcp9808(reg, tmp);
        break;
    case CHIP_INA226:
        w = model_ina226(reg, tmp);
        break;
    case CHIP_AHT10:
        w = model_aht10(tmp, len);
        break;
    case CHIP_PMSA003I:
        w = model_pmsa003i(tmp, len);
        break;
    default:
        return -1;
    }

    for (size_t i = 0; i < len; i++)
        out[i] = i < w ? tmp[i] : 0x00;
    return (int)len;
}

static void chip_write(int addr, int reg, const uint8_t *data, size_t len)
{
    if (addr < 0 || addr > 127)
        return;
    uint16_t v = 0;
    if (len >= 2)
        v = (uint16_t)((data[0] << 8) | data[1]);
    else if (len == 1)
        v = data[0];
    g_shadow[addr][reg & 0xFF] = v;
}

/* ---- fd slot bookkeeping ------------------------------------------------ */
static struct slot *slot_find(int fd)
{
    for (int i = 0; i < MAX_SLOTS; i++)
        if (g_slots[i].fd == fd)
            return &g_slots[i];
    return NULL;
}

static int fake_open(const char *path)
{
    int fd = r_open64 ? r_open64("/dev/null", O_RDWR | O_CLOEXEC) : r_open("/dev/null", O_RDWR | O_CLOEXEC);
    if (fd < 0)
        return -1;
    pthread_mutex_lock(&g_lock);
    struct slot *s = slot_find(-1);
    if (!s) {
        pthread_mutex_unlock(&g_lock);
        r_close(fd);
        errno = ENFILE;
        return -1;
    }
    s->fd = fd;
    s->addr = -1;
    s->reg = 0;
    s->reg_valid = 0;
    pthread_mutex_unlock(&g_lock);
    trace("[i2cshim] open(%s) -> fake fd %d\n", path, fd);
    return fd;
}

/* ---- interposed symbols ------------------------------------------------- */
#define OPEN_MODE(flags)                                                                                               \
    ({                                                                                                                 \
        mode_t _m = 0;                                                                                                 \
        if ((flags) & (O_CREAT | O_TMPFILE)) {                                                                          \
            va_list _ap;                                                                                               \
            va_start(_ap, flags);                                                                                       \
            _m = va_arg(_ap, mode_t);                                                                                   \
            va_end(_ap);                                                                                               \
        }                                                                                                              \
        _m;                                                                                                            \
    })

int open(const char *path, int flags, ...)
{
    if (path_is_fake(path))
        return fake_open(path);
    mode_t mode = OPEN_MODE(flags);
    return r_open(path, flags, mode);
}

int open64(const char *path, int flags, ...)
{
    if (path_is_fake(path))
        return fake_open(path);
    mode_t mode = OPEN_MODE(flags);
    return r_open64 ? r_open64(path, flags, mode) : r_open(path, flags, mode);
}

int openat(int dirfd, const char *path, int flags, ...)
{
    if (path_is_fake(path))
        return fake_open(path);
    mode_t mode = OPEN_MODE(flags);
    return r_openat(dirfd, path, flags, mode);
}

int openat64(int dirfd, const char *path, int flags, ...)
{
    if (path_is_fake(path))
        return fake_open(path);
    mode_t mode = OPEN_MODE(flags);
    return r_openat64 ? r_openat64(dirfd, path, flags, mode) : r_openat(dirfd, path, flags, mode);
}

int close(int fd)
{
    pthread_mutex_lock(&g_lock);
    struct slot *s = slot_find(fd);
    if (s)
        s->fd = -1;
    pthread_mutex_unlock(&g_lock);
    return r_close(fd);
}

/* Shared body for read() and __read_chk(). Returns 1 if this fd is ours -- and
 * then *out is the result -- or 0 to fall through to libc. */
static int shim_read(int fd, void *buf, size_t count, ssize_t *out)
{
    pthread_mutex_lock(&g_lock);
    struct slot *s = slot_find(fd);
    if (!s) {
        pthread_mutex_unlock(&g_lock);
        return 0;
    }
    int addr = s->addr;
    int reg = s->reg_valid ? s->reg : 0x00;
    pthread_mutex_unlock(&g_lock);

    if (chip_at(addr) < 0) {
        trace("[i2cshim] read(addr=0x%02x len=%zu) -> ENXIO\n", addr, count);
        errno = ENXIO;
        *out = -1;
        return 1;
    }
    /* Portduino's bare-read scan probe and its requestFrom() fallback both land
     * here. A real chip answers from its current register pointer. */
    uint8_t tmp[MAX_READ];
    size_t n = count > sizeof(tmp) ? sizeof(tmp) : count;
    if (chip_read(addr, reg, tmp, n) < 0) {
        errno = ENXIO;
        *out = -1;
        return 1;
    }
    memcpy(buf, tmp, n);
    trace("[i2cshim] read(addr=0x%02x reg=0x%02x len=%zu) -> %02x %02x\n", addr, reg, n, tmp[0],
          n > 1 ? tmp[1] : 0);
    *out = (ssize_t)n;
    return 1;
}

ssize_t read(int fd, void *buf, size_t count)
{
    ssize_t rc;
    if (shim_read(fd, buf, count, &rc))
        return rc;
    return r_read(fd, buf, count);
}

/* See gotcha 1 in the header comment: meshtasticd is FORTIFY-hardened, so
 * Portduino's ::read(i2c_file, RXbuf, count) compiles to this, not to read().
 * Interposing read() alone makes the scan work and every reading zero. */
ssize_t __read_chk(int fd, void *buf, size_t count, size_t buflen)
{
    ssize_t rc;
    if (count <= buflen && shim_read(fd, buf, count, &rc))
        return rc;
    if (r_read_chk)
        return r_read_chk(fd, buf, count, buflen);
    return r_read(fd, buf, count);
}

ssize_t write(int fd, const void *buf, size_t count)
{
    pthread_mutex_lock(&g_lock);
    struct slot *s = slot_find(fd);
    if (!s) {
        pthread_mutex_unlock(&g_lock);
        return r_write(fd, buf, count);
    }
    int addr = s->addr;
    int emulated = chip_at(addr) >= 0;
    if (emulated && count >= 1) {
        const uint8_t *b = buf;
        s->reg = b[0];
        s->reg_valid = 1;
        if (count > 1)
            chip_write(addr, b[0], b + 1, count - 1);
    }
    pthread_mutex_unlock(&g_lock);

    if (!emulated) {
        trace("[i2cshim] write(addr=0x%02x len=%zu) -> ENXIO\n", addr, count);
        errno = ENXIO;
        return -1;
    }
    trace("[i2cshim] write(addr=0x%02x reg=0x%02x len=%zu) -> ok\n", addr,
          count ? ((const uint8_t *)buf)[0] : 0, count);
    return (ssize_t)count;
}

/* A combined write-then-read transfer: Adafruit_BusIO's write_then_read() and
 * Portduino's requestFrom() when a write is already buffered. */
static int do_rdwr(struct i2c_rdwr_ioctl_data *d, struct slot *s)
{
    if (!d || !d->msgs || d->nmsgs == 0 || d->nmsgs > 8) {
        errno = EINVAL;
        return -1;
    }
    /* Every message must target an emulated chip, else NAK the whole transfer. */
    for (uint32_t i = 0; i < d->nmsgs; i++)
        if (chip_at(d->msgs[i].addr) < 0) {
            trace("[i2cshim] I2C_RDWR addr=0x%02x -> ENXIO\n", d->msgs[i].addr);
            errno = ENXIO;
            return -1;
        }

    int reg = s->reg_valid ? s->reg : 0x00;
    for (uint32_t i = 0; i < d->nmsgs; i++) {
        struct i2c_msg *m = &d->msgs[i];
        if (!m->buf && m->len) {
            errno = EFAULT;
            return -1;
        }
        if (m->flags & I2C_M_RD) {
            if (chip_read(m->addr, reg, m->buf, m->len) < 0) {
                errno = ENXIO;
                return -1;
            }
            trace("[i2cshim] RDWR rd addr=0x%02x reg=0x%02x len=%u -> %02x %02x\n", m->addr, reg, m->len,
                  m->len > 0 ? m->buf[0] : 0, m->len > 1 ? m->buf[1] : 0);
        } else if (m->len >= 1) {
            reg = m->buf[0];
            s->reg = m->buf[0];
            s->reg_valid = 1;
            if (m->len > 1)
                chip_write(m->addr, m->buf[0], m->buf + 1, m->len - 1);
            trace("[i2cshim] RDWR wr addr=0x%02x reg=0x%02x len=%u\n", m->addr, reg, m->len);
        }
    }
    return (int)d->nmsgs;
}

static int do_smbus(struct i2c_smbus_ioctl_data *d, struct slot *s)
{
    if (chip_at(s->addr) < 0) {
        trace("[i2cshim] SMBUS size=%u addr=0x%02x -> ENXIO\n", d ? d->size : 0, s->addr);
        errno = ENXIO;
        return -1;
    }
    if (!d) {
        errno = EINVAL;
        return -1;
    }
    if (d->size == I2C_SMBUS_QUICK)
        return 0; /* ACK: this is how the firmware scan probes most addresses */

    if (d->read_write == I2C_SMBUS_READ) {
        if (!d->data) {
            errno = EINVAL;
            return -1;
        }
        uint8_t tmp[2] = {0, 0};
        int reg = (d->size == I2C_SMBUS_BYTE) ? (s->reg_valid ? s->reg : 0) : d->command;
        if (chip_read(s->addr, reg, tmp, 2) < 0) {
            errno = ENXIO;
            return -1;
        }
        if (d->size == I2C_SMBUS_WORD_DATA)
            d->data->word = (uint16_t)((tmp[1] << 8) | tmp[0]); /* an SMBus word is little-endian */
        else
            d->data->byte = tmp[0];
        return 0;
    }

    if (d->size == I2C_SMBUS_BYTE) {
        s->reg = d->command;
        s->reg_valid = 1;
    } else if (d->data) {
        uint8_t b[2];
        size_t n = (d->size == I2C_SMBUS_WORD_DATA) ? 2 : 1;
        if (n == 2) {
            b[0] = (uint8_t)(d->data->word >> 8);
            b[1] = (uint8_t)(d->data->word & 0xFF);
        } else {
            b[0] = d->data->byte;
        }
        chip_write(s->addr, d->command, b, n);
        s->reg = d->command;
        s->reg_valid = 1;
    }
    return 0;
}

int ioctl(int fd, unsigned long request, ...)
{
    va_list ap;
    va_start(ap, request);
    void *arg = va_arg(ap, void *);
    va_end(ap);

    pthread_mutex_lock(&g_lock);
    struct slot *s = slot_find(fd);
    if (!s) {
        pthread_mutex_unlock(&g_lock);
        return r_ioctl(fd, request, arg);
    }

    int rc;
    if (request != I2C_SMBUS)
        trace("[i2cshim] ioctl(req=0x%04lx addr=0x%02x)\n", request, s->addr);

    switch (request) {
    case I2C_SLAVE:
    case I2C_SLAVE_FORCE: {
        int newaddr = (int)(uintptr_t)arg;
        /* Gotcha 2: a real chip keeps its register pointer across a repeated
         * START, and Portduino's getRegisterValue() depends on exactly that --
         * write(reg) + endTransmission(true), then requestFrom() re-issues
         * I2C_SLAVE before the bare read(). Only a change of target resets it. */
        if (newaddr != s->addr)
            s->reg_valid = 0;
        s->addr = newaddr;
        /* Portduino ignores this, but a real driver would see the NAK. */
        if (chip_at(s->addr) < 0) {
            pthread_mutex_unlock(&g_lock);
            errno = ENXIO;
            return -1;
        }
        rc = 0;
        break;
    }
    case I2C_TENBIT:
    case I2C_PEC:
        rc = 0;
        break;
    case I2C_FUNCS:
        if (arg)
            *(unsigned long *)arg = 0x00000001UL   /* I2C            */
                                    | 0x00010000UL /* SMBUS_QUICK    */
                                    | 0x00020000UL | 0x00040000UL /* R/W BYTE      */
                                    | 0x00080000UL | 0x00100000UL /* R/W BYTE_DATA */
                                    | 0x00200000UL | 0x00400000UL /* R/W WORD_DATA */;
        rc = 0;
        break;
    case I2C_RDWR:
        rc = do_rdwr((struct i2c_rdwr_ioctl_data *)arg, s);
        break;
    case I2C_SMBUS:
        rc = do_smbus((struct i2c_smbus_ioctl_data *)arg, s);
        break;
    case FIONREAD:
        /* Portduino only consults this when its own RX buffer is empty. */
        if (arg)
            *(int *)arg = 0;
        rc = 0;
        break;
    default:
        rc = 0; /* swallow the rest rather than confuse the caller */
        break;
    }
    pthread_mutex_unlock(&g_lock);
    return rc;
}
