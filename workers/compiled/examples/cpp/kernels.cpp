// Example kernel table for the rbw1 harness (C++). Replace with Boost.Math or
// any C/C++ library: one entry per function the cases may ask for.
#include <cmath>
#include <cstdint>

extern "C" {
typedef void (*rbw1_fn)(int64_t n, const double *const *cols, double *out);
struct rbw1_kernel {
	const char *name;
	rbw1_fn fn;
};

static void sqrt_k(int64_t n, const double *const *cols, double *out) {
	for (int64_t i = 0; i < n; i++) out[i] = std::sqrt(cols[0][i]);
}

static void add_k(int64_t n, const double *const *cols, double *out) {
	for (int64_t i = 0; i < n; i++) out[i] = cols[0][i] + cols[1][i];
}

extern const rbw1_kernel rbw1_kernels[] = {{"sqrt", sqrt_k}, {"add", add_k}, {nullptr, nullptr}};
}
