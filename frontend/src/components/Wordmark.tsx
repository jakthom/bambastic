import lockup from '../assets/brand/bambastic-lockup.svg?no-inline';
import mark from '../assets/brand/bambastic-mark.svg';

type BrandVariant = 'header' | 'compact' | 'auth';

export function Wordmark({ variant = 'header' }: { variant?: BrandVariant }) {
  if (variant === 'compact') {
    return <img className="brand-mark" src={mark} alt="Bambastic" />;
  }

  return (
    <svg
      className={variant === 'auth' ? 'brand-wordmark brand-wordmark--auth' : 'brand-wordmark'}
      viewBox="0 0 1744 355"
      role="img"
      aria-label="Bambastic"
      focusable="false"
    >
      <use href={`${lockup}#lockup`} />
    </svg>
  );
}
