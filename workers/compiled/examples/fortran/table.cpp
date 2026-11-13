// Kernel table of the Fortran example: the entries are Fortran subroutines
// declared bind(C) (see sqrt.f90).
#include <cstdint>

extern "C" {
typedef void (*rbw1_fn)(int64_t n, const double *const *cols, double *out);
struct rbw1_kernel {
	const char *name;
	rbw1_fn fn;
};

void sqrt_f(int64_t n, const double *const *cols, double *out);

extern const rbw1_kernel rbw1_kernels[] = {{"sqrt", sqrt_f}, {nullptr, nullptr}};
}
