import { memo } from "react";
import { promoItems } from "@/data/promos";

const kindLabel: Record<string, string> = {
  channel: "频道",
  topic: "专题",
  event: "活动",
};

export const PromoStrip = memo(function PromoStrip() {
  if (promoItems.length === 0) return null;
  return (
    <div className="promo-strip" aria-label="推荐内容">
      {promoItems.map((p) => (
        <a key={p.id} className="promo-card" href={`#${p.id}`}>
          <span className="promo-card__label">
            {p.label} · {kindLabel[p.kind]}
          </span>
          <span className="promo-card__title">{p.title}</span>
          {p.meta && <span className="promo-card__meta">{p.meta}</span>}
        </a>
      ))}
    </div>
  );
});
