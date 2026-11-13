// rbw1 worker harness for compiled references (C, C++, Fortran, Boost.Math).
// See docs/design/multiref.md, section 3.3. Build with refbench.BuildWorker:
//
//     c++ -O2 -std=c++17 -o worker rbw1.cpp kernels.cpp [kernels.f90 ...]
//
// The kernels are the worker's fixed function table: a program supplies
//
//     extern "C" const rbw1_kernel rbw1_kernels[] = { {"sqrt", sqrt_k}, {nullptr, nullptr} };
//
// where a kernel evaluates n points, column j of the arguments being cols[j][0..n):
//
//     typedef void (*rbw1_fn)(int64_t n, const double *const *cols, double *out);
//
// A Fortran kernel is `subroutine k(n, cols, out) bind(C, name="k")` with
// n of kind c_int64_t, value, and cols a type(c_ptr) array (see
// examples/fortran). Nothing read from stdin is evaluated: a request names an
// entry of rbw1_kernels. Paths must lie inside $RBW1_DIR. No threads are
// started; the checksum of every call is kept so that no loop can be removed.
#include <chrono>
#include <cfloat>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <map>
#include <string>
#include <vector>
#ifdef _OPENMP
#include <omp.h>
#endif

extern "C" {
typedef void (*rbw1_fn)(int64_t n, const double *const *cols, double *out);
struct rbw1_kernel {
	const char *name;
	rbw1_fn fn;
};
extern const rbw1_kernel rbw1_kernels[];
}

namespace {

const size_t kMaxLine = 4096;
const int64_t kMaxData = 256LL << 20;

// SHA-256 (FIPS 180-4).
struct Sha256 {
	uint32_t h[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};
	uint8_t buf[64];
	uint64_t len = 0;
	size_t n = 0;
	static uint32_t rotr(uint32_t x, int k) { return (x >> k) | (x << (32 - k)); }
	void block(const uint8_t *p) {
		static const uint32_t K[64] = {
		    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
		    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
		    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
		    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
		    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
		    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
		    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
		    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2};
		uint32_t w[64];
		for (int i = 0; i < 16; i++)
			w[i] = (uint32_t)p[4 * i] << 24 | (uint32_t)p[4 * i + 1] << 16 | (uint32_t)p[4 * i + 2] << 8 | p[4 * i + 3];
		for (int i = 16; i < 64; i++) {
			uint32_t s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
			uint32_t s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
			w[i] = w[i - 16] + s0 + w[i - 7] + s1;
		}
		uint32_t a = h[0], b = h[1], c = h[2], d = h[3], e = h[4], f = h[5], g = h[6], hh = h[7];
		for (int i = 0; i < 64; i++) {
			uint32_t S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25), ch = (e & f) ^ (~e & g);
			uint32_t t1 = hh + S1 + ch + K[i] + w[i];
			uint32_t S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22), mj = (a & b) ^ (a & c) ^ (b & c);
			uint32_t t2 = S0 + mj;
			hh = g; g = f; f = e; e = d + t1; d = c; c = b; b = a; a = t1 + t2;
		}
		h[0] += a; h[1] += b; h[2] += c; h[3] += d; h[4] += e; h[5] += f; h[6] += g; h[7] += hh;
	}
	void update(const uint8_t *p, size_t m) {
		len += m;
		while (m > 0) {
			size_t k = 64 - n < m ? 64 - n : m;
			memcpy(buf + n, p, k);
			n += k; p += k; m -= k;
			if (n == 64) { block(buf); n = 0; }
		}
	}
	std::string hex() {
		uint64_t bits = len * 8;
		uint8_t pad = 0x80;
		update(&pad, 1);
		pad = 0;
		while (n != 56) update(&pad, 1);
		uint8_t l[8];
		for (int i = 0; i < 8; i++) l[i] = (uint8_t)(bits >> (56 - 8 * i));
		update(l, 8);
		char out[65];
		for (int i = 0; i < 8; i++) snprintf(out + 8 * i, 9, "%08x", h[i]);
		return std::string(out, 64);
	}
};

struct Data {
	std::vector<double> v;
	std::vector<const double *> cols;
	std::vector<double> out;
	int64_t rows = 0;
};

std::map<std::string, Data> g_data;
std::string g_dir;
volatile double g_sink = 0;

void reply(const std::string &s) {
	fputs(s.c_str(), stdout);
	fputc('\n', stdout);
	fflush(stdout);
}

void fail(const std::string &m) {
	std::string s = "err ";
	for (char c : m.substr(0, 200)) s += (c >= 0x20 && c <= 0x7e) ? c : '?';
	reply(s);
}

bool split(const std::string &line, std::vector<std::string> &t) {
	size_t i = 0;
	while (true) {
		size_t j = line.find(' ', i);
		std::string tok = line.substr(i, j == std::string::npos ? std::string::npos : j - i);
		if (tok.empty()) return false;
		t.push_back(tok);
		if (j == std::string::npos) return true;
		i = j + 1;
	}
}

bool inside(const std::string &p) {
	return p.size() > g_dir.size() + 1 && p.compare(0, g_dir.size() + 1, g_dir + "/") == 0 && p.find("..") == std::string::npos;
}

bool parseInt(const std::string &s, int64_t &v) {
	char *end = nullptr;
	if (s.empty() || s.size() > 18) return false;
	v = strtoll(s.c_str(), &end, 10);
	return *end == 0;
}

rbw1_fn find(const std::string &name) {
	for (const rbw1_kernel *k = rbw1_kernels; k->name; k++)
		if (name == k->name) return k->fn;
	return nullptr;
}

// threadInfo reports what the harness can observe, and says where it comes from:
// the OpenMP maximum when the worker is built with it, else (Linux) the number
// of threads of this process now, else an assumption ("assumed": the harness
// starts no thread, but a linked library that does so is NOT detected there).
std::string threadInfo() {
#ifdef _OPENMP
	return "threads=" + std::to_string(omp_get_max_threads()) + " threads_source=omp";
#elif defined(__linux__)
	FILE *f = fopen("/proc/self/status", "r");
	if (f) {
		char l[256];
		while (fgets(l, sizeof l, f)) {
			long n;
			if (sscanf(l, "Threads: %ld", &n) == 1) {
				fclose(f);
				return "threads=" + std::to_string(n) + " threads_source=proc";
			}
		}
		fclose(f);
	}
#endif
	return "threads=1 threads_source=assumed";
}

void call(rbw1_fn f, Data &d) {
	f(d.rows, d.cols.data(), d.out.data());
	g_sink = g_sink + d.out[0];
}

void handle(const std::string &line) {
	std::vector<std::string> t;
	if (line.size() > kMaxLine || !split(line, t)) return fail("malformed line");
	const std::string &c = t[0];
	if (c == "L" && t.size() == 5) {
		int64_t cols, rows;
		if (!parseInt(t[3], cols) || !parseInt(t[4], rows) || cols <= 0 || rows <= 0 || cols > (1 << 28) || rows > (1 << 28) ||
		    cols * rows * 8 > kMaxData)
			return fail("size out of range");
		if (!inside(t[2])) return fail("path outside the run directory");
		FILE *f = fopen(t[2].c_str(), "rb");
		if (!f) return fail("cannot open the file");
		Data d;
		d.v.resize((size_t)(cols * rows));
		size_t got = fread(d.v.data(), 8, d.v.size(), f);
		int extra = fgetc(f);
		fclose(f);
		if (got != d.v.size() || extra != EOF) return fail("file size differs from cols*rows*8");
		const uint16_t one = 1;
		if (*(const uint8_t *)&one != 1) return fail("big-endian host");
		Sha256 s;
		s.update((const uint8_t *)d.v.data(), d.v.size() * 8);
		d.rows = rows;
		for (int64_t j = 0; j < cols; j++) d.cols.push_back(d.v.data() + j * rows);
		d.out.assign((size_t)rows, 0.0);
		g_data[t[1]] = std::move(d);
		return reply("ok " + s.hex());
	}
	if ((c == "W" && t.size() == 3) || (c == "T" && t.size() == 4) || (c == "E" && t.size() == 4)) {
		auto it = g_data.find(t[1]);
		if (it == g_data.end()) return fail("key not loaded");
		rbw1_fn f = find(t[2]);
		if (!f) return fail("function not in the table");
		Data &d = it->second;
		if (c == "W") {
			call(f, d);
			return reply("ok");
		}
		if (c == "T") {
			int64_t iters;
			if (!parseInt(t[3], iters) || iters <= 0) return fail("iters must be positive");
			auto t0 = std::chrono::steady_clock::now();
			for (int64_t i = 0; i < iters; i++) call(f, d);
			auto t1 = std::chrono::steady_clock::now();
			char b[64];
			snprintf(b, sizeof b, "ns %.1f", (double)std::chrono::duration_cast<std::chrono::nanoseconds>(t1 - t0).count());
			return reply(b);
		}
		if (!inside(t[3])) return fail("path outside the run directory");
		call(f, d);
		FILE *o = fopen(t[3].c_str(), "wb");
		if (!o) return fail("cannot create the file");
		size_t w = fwrite(d.out.data(), 8, d.out.size(), o);
		int cl = fclose(o);
		if (w != d.out.size() || cl != 0) return fail("cannot write the file");
		long nonfinite = 0;
		for (double x : d.out) nonfinite += !std::isfinite(x);
		return reply("ok " + std::to_string(nonfinite) + " 0");
	}
	if (c == "V" && t.size() == 1) {
#ifdef __VERSION__
		std::string ver = __VERSION__;
#else
		std::string ver = "unknown";
#endif
		for (char &ch : ver)
			if (ch == ' ') ch = '_';
		std::string s = "v compiler=" + ver + " " + threadInfo() + " ldbl_mant_dig=" + std::to_string(LDBL_MANT_DIG);
#ifdef RBW1_FLAGS
		std::string fl = RBW1_FLAGS;
		for (char &ch : fl)
			if (ch == ' ') ch = '_';
		s += " flags=" + fl;
#endif
#ifdef BOOST_VERSION
		s += " boost=" + std::to_string(BOOST_VERSION);
#endif
		return reply(s);
	}
	fail("unknown request");
}

}  // namespace

int main() {
	const char *dir = getenv("RBW1_DIR");
	if (!dir || !*dir) {
		fputs("err RBW1_DIR is not set\n", stdout);
		return 1;
	}
	g_dir = dir;
	std::string line;
	int ch;
	while (true) {
		line.clear();
		bool over = false;
		while ((ch = getchar()) != EOF && ch != '\n') {
			if (line.size() <= kMaxLine) line += (char)ch;
			else over = true;
		}
		if (ch == EOF && line.empty()) break;
		if (over) fail("line longer than 4096 bytes");
		else handle(line);
	}
	return 0;
}
