! Example Fortran kernel for the rbw1 harness: AMOS, QUADPACK or MINPACK
! routines are called from subroutines of this shape.
subroutine sqrt_f(n, cols, out) bind(C, name="sqrt_f")
  use, intrinsic :: iso_c_binding
  implicit none
  integer(c_int64_t), value :: n
  type(c_ptr), intent(in) :: cols(*)
  real(c_double), intent(out) :: out(*)
  real(c_double), pointer :: x(:)
  integer(c_int64_t) :: i
  call c_f_pointer(cols(1), x, [n])
  do i = 1, n
     out(i) = sqrt(x(i))
  end do
end subroutine sqrt_f
