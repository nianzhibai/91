import { useEffect, useState } from "react";
import { ArrowUp } from "lucide-react";

type Props = {
  onVisibilityChange?: (visible: boolean) => void;
};

/**
 * 虚拟列表会在滚动过程中按实测行高做位置补偿，平滑滚动动画会被这些补偿
 * 打断、停在半路，所以返回顶部直接落到 0。
 */
function scrollToTop() {
  window.scrollTo({ top: 0, behavior: "auto" });
}

export function BackToTop({ onVisibilityChange }: Props) {
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    function onScroll() {
      const nextVisible = window.scrollY > 400;
      setVisible((current) => {
        if (current !== nextVisible) {
          onVisibilityChange?.(nextVisible);
        }
        return nextVisible;
      });
    }
    window.addEventListener("scroll", onScroll, { passive: true });
    onScroll();
    return () => window.removeEventListener("scroll", onScroll);
  }, [onVisibilityChange]);

  return (
    <button
      className={`back-to-top ${visible ? "is-visible" : ""}`}
      onClick={scrollToTop}
      aria-label="返回顶部"
    >
      <ArrowUp size={18} />
    </button>
  );
}
