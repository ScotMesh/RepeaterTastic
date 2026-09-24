/*
 * replay.c -- the shim's self-test.
 *
 * Replays the exact call sequences meshtasticd makes, with no container and no
 * meshtasticd: a faithful copy of Portduino's LinuxHardwareI2C, the firmware's
 * 112-address scan (including its bare-read probe range 0x30-0x37), the
 * firmware's own getRegisterValue(), Adafruit_BusIO's combined write-then-read,
 * the AHT10 command sequence, the PMSA003I frame read, register-pointer
 * persistence across a repeated START, and a direct __read_chk() call.
 *
 * It asserts the decoded readings against the values file, rewrites that file
 * and asserts the new readings, and exits non-zero on any failure. See test.sh.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>

/* The i2c-dev ABI, inlined to match i2cshim.c and to need no kernel headers. */
#define I2C_SLAVE 0x0703
#define I2C_RDWR 0x0707
#define I2C_SMBUS 0x0720
#define I2C_M_RD 0x0001
#define I2C_SMBUS_QUICK 0

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

extern ssize_t __read_chk(int fd, void *buf, size_t count, size_t buflen);

/* ---- results ------------------------------------------------------------- */
static int g_fail, g_pass;

static void ok(int cond, const char *what, const char *detail)
{
    if (cond) {
        g_pass++;
        printf("  ok   %-52s %s\n", what, detail);
    } else {
        g_fail++;
        printf("  FAIL %-52s %s\n", what, detail);
    }
}

/* The condition is evaluated first, deliberately: it is usually the call that
 * produces the bytes the detail string then reports. */
#define OKF(cond, what, ...)                                                                                           \
    do {                                                                                                               \
        int _c = (cond);                                                                                               \
        char _d[160];                                                                                                  \
        snprintf(_d, sizeof(_d), __VA_ARGS__);                                                                         \
        ok(_c, (what), _d);                                                                                            \
    } while (0)

static int near(double a, double b, double tol) { return fabs(a - b) <= tol; }

/* ---- Portduino LinuxHardwareI2C, copied ---------------------------------- */
static int fd;
static char TXbuf[1000], RXbuf[1000];
static int RXlen, requestedBytes;
static size_t RXindex;

static void beginTransmission(uint8_t a)
{
    ioctl(fd, I2C_SLAVE, a);
    requestedBytes = 0;
}
static void w8(uint8_t v) { TXbuf[requestedBytes++] = v; }
static uint8_t endTransmission(int stop)
{
    if (stop && requestedBytes) {
        int r = (int)write(fd, TXbuf, requestedBytes);
        int good = r == requestedBytes;
        requestedBytes = 0;
        return good ? 0 : 4;
    }
    return 0;
}
static int rd(void)
{
    if (RXlen - (int)RXindex != 0) {
        int v = (uint8_t)RXbuf[RXindex++];
        if ((int)RXindex == RXlen) {
            RXindex = 0;
            RXlen = 0;
        }
        return v;
    }
    int tmp = 0;
    if (read(fd, &tmp, 1) == -1)
        return -1;
    return tmp;
}
static int available(void)
{
    if (RXlen - (int)RXindex != 0)
        return RXlen - (int)RXindex;
    int n = 0;
    ioctl(fd, FIONREAD, &n);
    return n;
}
static int writeQuick(uint8_t v)
{
    struct i2c_smbus_ioctl_data a = {.read_write = v, .command = 0, .size = I2C_SMBUS_QUICK, .data = NULL};
    return ioctl(fd, I2C_SMBUS, &a);
}
static int requestFrom(uint8_t addr, size_t count)
{
    if (requestedBytes) {
        struct i2c_msg m[2] = {{.addr = addr, .flags = 0, .len = (uint16_t)requestedBytes, .buf = (uint8_t *)TXbuf},
                               {.addr = addr, .flags = I2C_M_RD, .len = (uint16_t)count, .buf = (uint8_t *)RXbuf}};
        struct i2c_rdwr_ioctl_data d = {.msgs = m, .nmsgs = 2};
        int r = ioctl(fd, I2C_RDWR, &d);
        if (r >= 0) {
            RXlen = (int)count;
            RXindex = 0;
            return (int)count;
        }
        return r;
    }
    ioctl(fd, I2C_SLAVE, addr);
    RXlen = (int)read(fd, RXbuf, count);
    if (RXlen < 1)
        RXlen = 0;
    RXindex = 0;
    return RXlen;
}

/* ---- the two register-read paths ---------------------------------------- */
/* Adafruit_BusIO_Register::read() -> write_then_read(stop=false): one combined
 * I2C_RDWR transfer, because endTransmission(false) buffers the register. */
static uint32_t busio_read(uint8_t addr, uint8_t reg, uint8_t width)
{
    beginTransmission(addr);
    w8(reg);
    endTransmission(0);
    if (requestFrom(addr, width) != width)
        return (uint32_t)-1;
    uint32_t v = 0;
    for (int i = 0; i < width; i++) {
        v <<= 8;
        v |= (uint8_t)rd();
    }
    return v;
}

/* ScanI2CTwoWire::getRegisterValue(): endTransmission(TRUE) really writes, so
 * requestFrom() takes the separate ioctl(I2C_SLAVE) + bare read() path. */
static uint16_t getRegisterValue(uint8_t addr, uint8_t reg, uint8_t width)
{
    beginTransmission(addr);
    w8(reg);
    endTransmission(1);
    requestFrom(addr, width);
    uint16_t v = 0;
    if (RXlen - (int)RXindex > 1) {
        v = (uint16_t)((uint8_t)rd() << 8);
        v |= (uint8_t)rd();
    } else if (RXlen - (int)RXindex) {
        v = (uint8_t)rd();
    }
    for (int i = 0; i < width - 1; i++)
        if (RXlen - (int)RXindex)
            rd();
    return v;
}

/* Adafruit_I2CDevice::write() / ::read(), as the AHTX0 driver uses them. */
static int dev_write(uint8_t addr, const uint8_t *b, size_t n)
{
    beginTransmission(addr);
    for (size_t i = 0; i < n; i++)
        w8(b[i]);
    return endTransmission(1) == 0;
}
static int dev_read(uint8_t addr, uint8_t *b, size_t n)
{
    if ((size_t)requestFrom(addr, n) != n)
        return 0;
    for (size_t i = 0; i < n; i++)
        b[i] = (uint8_t)rd();
    return 1;
}

/* ---- the values file ---------------------------------------------------- */
static const char *g_valpath;

static void put_values(const char *body)
{
    FILE *f = fopen(g_valpath, "w");
    if (!f) {
        fprintf(stderr, "cannot write %s\n", g_valpath);
        exit(2);
    }
    fputs(body, f);
    fclose(f);
}

/* ---- the scan ----------------------------------------------------------- */
static int scan(uint8_t *found, int max)
{
    int n = 0;
    for (int a = 8; a < 120; a++) {
        int err = 2;
        beginTransmission((uint8_t)a);
        if ((a >= 0x30 && a <= 0x37) || (a >= 0x50 && a <= 0x5F)) {
            if (rd() != -1)
                err = 0;
        } else {
            err = writeQuick(0);
        }
        if (err != 0)
            err = 2;
        if (err == 0 && n < max)
            found[n++] = (uint8_t)a;
    }
    return n;
}

/* ---- checks ------------------------------------------------------------- */
static void check_scan(void)
{
    printf("\n== firmware I2C scan (112 addresses, portduino branch) ==\n");
    uint8_t f[128];
    int n = scan(f, (int)sizeof(f));
    char list[256] = "";
    for (int i = 0; i < n; i++)
        snprintf(list + strlen(list), sizeof(list) - strlen(list), "0x%02x ", f[i]);
    OKF(n == 5, "exactly the five emulated chips ACK", "got %d: %s", n, list);

    const uint8_t want[5] = {0x12, 0x18, 0x37, 0x38, 0x40};
    for (int i = 0; i < 5; i++) {
        int seen = 0;
        for (int j = 0; j < n; j++)
            if (f[j] == want[i])
                seen = 1;
        OKF(seen, "chip ACKs during the scan", "0x%02x", want[i]);
    }
    /* 0x37 is inside the bare-read probe range, so it is found by read(), not
     * by writeQuick -- the one case the scan probes differently. */
    beginTransmission(0x37);
    OKF(rd() != -1, "bare-read probe (0x30-0x37 range) answers", "0x37 via read()");
    beginTransmission(0x36);
    OKF(rd() == -1, "bare-read probe of an empty address fails", "0x36 -> -1");
}

static void check_pct2075(double want)
{
    printf("\n== PCT2075 @0x37 (temperature) ==\n");
    uint32_t raw = busio_read(0x37, 0x00, 2);
    double t = ((int16_t)raw >> 5) * 0.125;
    OKF(near(t, want, 0.0700), "combined I2C_RDWR path decodes temperature", "raw=0x%04x -> %.3f C (want %.2f)",
        raw, t, want);

    uint16_t raw2 = getRegisterValue(0x37, 0x00, 2);
    double t2 = ((int16_t)raw2 >> 5) * 0.125;
    OKF(near(t2, want, 0.0700), "separate write()+read() path decodes temperature", "raw=0x%04x -> %.3f C", raw2,
        t2);
}

static void check_ina226(double volts, double amps)
{
    printf("\n== INA226 @0x40 (voltage, current) ==\n");
    OKF(busio_read(0x40, 0xFE, 2) == 0x5449, "MFG_UID 0xFE == 0x5449 (combined)", "the scan insists on this");
    OKF(busio_read(0x40, 0xFF, 2) == 0x2260, "DIE_UID 0xFF == 0x2260 (combined)", "identifies an INA226");
    OKF(getRegisterValue(0x40, 0xFE, 2) == 0x5449, "MFG_UID 0xFE == 0x5449 (separate)", "the firmware's own path");
    OKF(getRegisterValue(0x40, 0xFF, 2) == 0x2260, "DIE_UID 0xFF == 0x2260 (separate)", "the firmware's own path");

    uint32_t bus = busio_read(0x40, 0x02, 2);
    double v = bus * 0.00125;
    OKF(near(v, volts, 0.002), "BUS_VOLTAGE decodes", "0x%04x -> %.4f V (want %.3f)", bus, v, volts);
    int16_t cur = (int16_t)busio_read(0x40, 0x04, 2);
    double a = cur * 2.5e-5;
    OKF(near(a, amps, 0.0005), "CURRENT decodes", "%d -> %.4f A (want %.3f)", cur, a, amps);
}

static void check_mcp9808(double want)
{
    printf("\n== MCP9808 @0x18 (temperature) ==\n");
    OKF(busio_read(0x18, 0x07, 2) == 0x0400, "DEVICE_ID 0x07 == 0x0400", "what the scan checks");
    OKF(busio_read(0x18, 0x06, 2) == 0x0054, "MFG_ID 0x06 == 0x0054", "Microchip");
    uint32_t m = busio_read(0x18, 0x05, 2);
    double t = (m & 0x0FFF) * 0.0625;
    if (m & 0x1000)
        t = -t;
    OKF(near(t, want, 0.0700), "AMBIENT_TEMP decodes", "0x%04x -> %.4f C (want %.2f)", m, t, want);
}

/* Adafruit_AHTX0::begin() then ::getEvent(), byte for byte. */
static void check_aht10(double hum, double temp)
{
    printf("\n== AHT10 @0x38 (humidity) ==\n");
    uint8_t cmd[3], st = 0;

    cmd[0] = 0xBA; /* SOFTRESET */
    OKF(dev_write(0x38, cmd, 1), "begin(): soft reset accepted", "write 0xBA");
    OKF(dev_read(0x38, &st, 1) && !(st & 0x80), "begin(): status is not BUSY", "status=0x%02x", st);

    cmd[0] = 0xE1; /* CALIBRATE */
    cmd[1] = 0x08;
    cmd[2] = 0x00;
    dev_write(0x38, cmd, 3);
    OKF(dev_read(0x38, &st, 1) && (st & 0x08), "begin(): status is CALIBRATED", "status=0x%02x (bit 3 set)", st);

    cmd[0] = 0xAC; /* TRIGGER */
    cmd[1] = 0x33;
    cmd[2] = 0x00;
    OKF(dev_write(0x38, cmd, 3), "getEvent(): trigger accepted", "write 0xAC 0x33 0x00");
    OKF(dev_read(0x38, &st, 1) && !(st & 0x80), "getEvent(): measurement ready", "status=0x%02x", st);

    uint8_t d[6] = {0};
    OKF(dev_read(0x38, d, 6), "getEvent(): six-byte measurement read", "%02x %02x %02x %02x %02x %02x", d[0], d[1],
        d[2], d[3], d[4], d[5]);

    uint32_t h = d[1];
    h <<= 8;
    h |= d[2];
    h <<= 4;
    h |= d[3] >> 4;
    double rh = (double)h * 100.0 / 0x100000;
    OKF(near(rh, hum, 0.01), "humidity decodes as the library does", "%.4f %% (want %.2f)", rh, hum);

    uint32_t tt = d[3] & 0x0F;
    tt <<= 8;
    tt |= d[4];
    tt <<= 8;
    tt |= d[5];
    double t = (double)tt * 200.0 / 0x100000 - 50.0;
    OKF(near(t, temp, 0.01), "temperature decodes as the library does", "%.4f C (want %.2f)", t, temp);
}

/* PMSA003ISensor::getMetrics(), byte for byte. */
static void check_pmsa003i(int pm10, int pm25, int pm100)
{
    printf("\n== PMSA003I @0x12 (pm10, pm25, pm100) ==\n");
    const int FRAME = 32;
    uint8_t b[32] = {0};

    beginTransmission(0x12); /* the driver does no write at all before this */
    requestedBytes = 0;
    requestFrom(0x12, FRAME);
    OKF(available() >= FRAME, "a bare 32-byte frame read returns a full frame", "available()=%d", available());
    for (int i = 0; i < FRAME; i++)
        b[i] = (uint8_t)rd();

    OKF(b[0] == 0x42 && b[1] == 0x4D, "frame header is 0x42 0x4D", "0x%02X 0x%02X", b[0], b[1]);

    uint16_t sum = 0;
    for (int i = 0; i < FRAME - 2; i++)
        sum = (uint16_t)(sum + b[i]);
    uint16_t got = (uint16_t)((b[FRAME - 2] << 8) | b[FRAME - 1]);
    OKF(sum == got, "additive checksum over the first 30 bytes matches", "computed 0x%04X, in frame 0x%04X", sum,
        got);

#define R16(i) ((uint16_t)((b[i] << 8) | b[(i) + 1]))
    OKF(R16(2) == FRAME - 4, "frame-length field is 28", "%u", R16(2));
    OKF(R16(4) == pm10, "pm10_standard at offset 4", "%u (want %d)", R16(4), pm10);
    OKF(R16(6) == pm25, "pm25_standard at offset 6", "%u (want %d)", R16(6), pm25);
    OKF(R16(8) == pm100, "pm100_standard at offset 8", "%u (want %d)", R16(8), pm100);
    OKF(R16(10) == pm10 && R16(12) == pm25 && R16(14) == pm100, "environmental copies at 10, 12, 14",
        "%u %u %u", R16(10), R16(12), R16(14));
#undef R16
}

static void check_regptr(void)
{
    printf("\n== register pointer across a repeated START ==\n");
    /* write(reg) then a *new* ioctl(I2C_SLAVE) for the same address, then a bare
     * read: a real chip still answers from the register we pointed at. */
    beginTransmission(0x40);
    w8(0xFE);
    endTransmission(1);
    ioctl(fd, I2C_SLAVE, 0x40);
    uint8_t b[2] = {0, 0};
    ssize_t n = read(fd, b, 2);
    uint16_t v = (uint16_t)((b[0] << 8) | b[1]);
    OKF(n == 2 && v == 0x5449, "pointer survives a repeated START to the same addr", "read -> 0x%04x", v);

    /* ... and a change of target resets it, so the next bare read is register 0. */
    ioctl(fd, I2C_SLAVE, 0x37);
    ioctl(fd, I2C_SLAVE, 0x40);
    b[0] = b[1] = 0;
    n = read(fd, b, 2);
    (void)n;
    v = (uint16_t)((b[0] << 8) | b[1]);
    OKF(v == 0x4127, "pointer resets when the target address changes", "read -> 0x%04x (CONFIG default)", v);
}

static void check_read_chk(void)
{
    printf("\n== __read_chk (FORTIFY) ==\n");
    /* meshtasticd is FORTIFY-hardened, so Portduino's read() of the RX buffer is
     * emitted as __read_chk. Call it directly: the shim must answer it too. */
    beginTransmission(0x40);
    w8(0xFF);
    endTransmission(1);
    ioctl(fd, I2C_SLAVE, 0x40);
    uint8_t b[8] = {0};
    ssize_t n = __read_chk(fd, b, 2, sizeof(b));
    uint16_t v = (uint16_t)((b[0] << 8) | b[1]);
    OKF(n == 2 && v == 0x2260, "__read_chk answers a register read", "-> 0x%04x (DIE_UID)", v);

    /* And a 32-byte PMSA003I frame through the same entry point. */
    ioctl(fd, I2C_SLAVE, 0x12);
    uint8_t f[64] = {0};
    n = __read_chk(fd, f, 32, sizeof(f));
    OKF(n == 32 && f[0] == 0x42 && f[1] == 0x4D, "__read_chk answers a 32-byte frame read", "n=%zd hdr=%02X %02X",
        n, f[0], f[1]);
}

static void check_nak(void)
{
    printf("\n== addresses we do not emulate ==\n");
    beginTransmission(0x50);
    w8(0);
    w8(0);
    OKF(endTransmission(1) == 4, "write to 0x50 fails (I2cOtherError)", "EEPROM range, not emulated");
    beginTransmission(0x50);
    OKF(requestFrom(0x50, 75) == 0, "requestFrom(0x50, 75) returns nothing", "no phantom EEPROM");
    beginTransmission(0x76);
    errno = 0;
    int q = writeQuick(0);
    OKF(q == -1 && errno == ENXIO, "writeQuick(0x76) NAKs with ENXIO", "rc=%d errno=%s", q, strerror(errno));
}

static void check_bad_input(void)
{
    printf("\n== a malformed values file never fails a transfer ==\n");
    put_values("this is not a key=value file\n"
               "= 5\n"
               "temperature\n"
               "unknown_key = 999\n"
               "humidity = potato\n"
               "# temperature = 99\n"
               "temperature = 11.25\n"
               "\"pm25\": 7,\n");
    uint32_t raw = busio_read(0x37, 0x00, 2);
    double t = ((int16_t)raw >> 5) * 0.125;
    OKF(near(t, 11.25, 0.0700), "good line among junk is still read", "%.3f C", t);

    uint8_t cmd[3] = {0xAC, 0x33, 0x00};
    dev_write(0x38, cmd, 3);
    uint8_t d[6] = {0};
    dev_read(0x38, d, 6);
    uint32_t h = ((uint32_t)d[1] << 12) | ((uint32_t)d[2] << 4) | (d[3] >> 4);
    double rh = (double)h * 100.0 / 0x100000;
    OKF(near(rh, 50.0, 0.01), "unparsable humidity falls back to the default", "%.3f %% (default 50)", rh);

    beginTransmission(0x12);
    requestedBytes = 0;
    requestFrom(0x12, 32);
    uint8_t b[32];
    for (int i = 0; i < 32; i++)
        b[i] = (uint8_t)rd();
    OKF(((b[6] << 8) | b[7]) == 7, "JSON-ish \"pm25\": 7, is accepted", "pm25=%u", (b[6] << 8) | b[7]);
    OKF(((b[4] << 8) | b[5]) == 0, "a field the file omits reads as 0", "pm10=%u", (b[4] << 8) | b[5]);
}

int main(int argc, char **argv)
{
    const char *dev = argc > 1 ? argv[1] : "/dev/i2c-fake";
    g_valpath = argc > 2 ? argv[2] : "sensor.kv";

    printf("i2cshim self-test: dev=%s values=%s\n", dev, g_valpath);

    put_values("# the shim re-reads this on every access\n"
               "temperature = 23.5\n"
               "humidity = 61.5\n"
               "voltage = 4.05\n"
               "current = 0.15\n"
               "pm10 = 12\n"
               "pm25 = 34\n"
               "pm100 = 56\n");

    fd = open(dev, O_RDWR);
    if (fd < 0) {
        fprintf(stderr, "open(%s) failed: %s -- is LD_PRELOAD and I2CSHIM_DEV set?\n", dev, strerror(errno));
        return 2;
    }
    printf("open(%s) -> fd %d\n", dev, fd);

    check_scan();
    check_pct2075(23.5);
    check_mcp9808(23.5);
    check_ina226(4.05, 0.15);
    check_aht10(61.5, 23.5);
    check_pmsa003i(12, 34, 56);
    check_regptr();
    check_read_chk();
    check_nak();

    /* The whole point: change the file, with nothing restarted, and the very
     * next read reports the new value. */
    printf("\n== the file changes and the next read follows it ==\n");
    put_values("temperature: -7.25\nhumidity: 88.0\npm10: 111\npm25: 222\npm100: 333\nvoltage: 3.70\n"
               "current: -0.25\n");
    check_pct2075(-7.25);
    check_mcp9808(-7.25);
    check_ina226(3.70, -0.25);
    check_aht10(88.0, -7.25);
    check_pmsa003i(111, 222, 333);

    check_bad_input();

    close(fd);
    printf("\n%d passed, %d failed\n", g_pass, g_fail);
    return g_fail ? 1 : 0;
}
