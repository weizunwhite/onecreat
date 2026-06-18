import { useEffect, useRef, useState } from "react";
import { Check, ChevronsUpDown, Coins } from "lucide-react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { useSession, setSessionStore } from "../lib/account";
import type { ModelInfo } from "../lib/types";

// ModelSwitcher is the bottom-of-window picker. Two modes:
//  - 普通(未登录/无档位):列出 config 里的模型,切换模型(原行为)。
//  - 网关订阅模式(已登录且平台配了档位):显示「档位」(标准/高级/旗舰)+ 剩余点数,
//    用户不知道背后是什么模型;切换调 SetOnecreatTier,由平台把档位映射到模型。
export function ModelSwitcher({ label, onPick }: { label: string; onPick: (name: string) => void }) {
  const t = useT();
  const session = useSession();
  const [open, setOpen] = useState(false);
  const [models, setModels] = useState<ModelInfo[]>([]);

  const tierMode = !!session?.loggedIn && (session.tiers?.length ?? 0) > 0;

  useEffect(() => {
    if (open && !tierMode) app.Models().then(setModels).catch(() => {});
  }, [open, tierMode]);

  // 本轮消耗:监测 points 下降显示"本轮 -N"(每轮结束 App 会向平台刷新点数)。
  const prevPoints = useRef<number | null>(null);
  const [lastUsed, setLastUsed] = useState(0);
  useEffect(() => {
    const p = session?.points ?? null;
    if (typeof p === "number" && typeof prevPoints.current === "number" && p < prevPoints.current) {
      setLastUsed(Math.round((prevPoints.current - p) * 10) / 10);
    }
    if (typeof p === "number") prevPoints.current = p;
  }, [session?.points]);

  // —— 网关订阅模式:档位 + 点数 ——
  if (tierMode && session) {
    const tiers = session.tiers;
    const sel = session.selectedTier;
    const curName = tiers.find((x) => x.index === sel)?.name ?? tiers[0]?.name ?? "档位";
    const pickTier = (index: number) => {
      setOpen(false);
      void app.SetOnecreatTier(index);
      setSessionStore({ ...session, selectedTier: index }); // 乐观更新
    };
    return (
      <div className="modelsw">
        <button className="modelsw__trigger" onClick={() => setOpen((v) => !v)} title="切换档位">
          <span className="modelsw__label">{curName}</span>
          {session.points === null ? (
            <span className="modelsw__points" title="超级管理员不限额度">
              不限
            </span>
          ) : (
            <span className="modelsw__points">
              <Coins size={10} /> {Math.round(session.points).toLocaleString()}
              {lastUsed > 0 && <em className="modelsw__used">本轮 -{lastUsed}</em>}
            </span>
          )}
          <ChevronsUpDown size={11} />
        </button>
        {open && (
          <>
            <div className="modelsw__backdrop" onClick={() => setOpen(false)} />
            <div className="modelsw__menu" role="listbox">
              {tiers.map((tier) => (
                <button
                  key={tier.index}
                  role="option"
                  aria-selected={tier.index === sel}
                  className={`modelsw__item ${tier.index === sel ? "modelsw__item--current" : ""}`}
                  onClick={() => pickTier(tier.index)}
                >
                  <span className="modelsw__model">{tier.name}</span>
                  {tier.index === sel && <Check size={13} className="modelsw__check" />}
                </button>
              ))}
            </div>
          </>
        )}
      </div>
    );
  }

  // —— 普通模式:模型选择(原行为)——
  const pick = (name: string) => {
    setOpen(false);
    onPick(name);
  };
  return (
    <div className="modelsw">
      <button className="modelsw__trigger" onClick={() => setOpen((v) => !v)} title={t("status.switchModel")}>
        <span className="modelsw__label">{label}</span>
        <ChevronsUpDown size={11} />
      </button>
      {open && (
        <>
          <div className="modelsw__backdrop" onClick={() => setOpen(false)} />
          <div className="modelsw__menu" role="listbox">
            {models.length === 0 && <div className="modelsw__empty">{t("status.noModels")}</div>}
            {models.map((m) => (
              <button
                key={m.ref}
                role="option"
                aria-selected={m.current}
                className={`modelsw__item ${m.current ? "modelsw__item--current" : ""}`}
                onClick={() => pick(m.ref)}
              >
                <span className="modelsw__model">{m.model}</span>
                {m.current && <Check size={13} className="modelsw__check" />}
              </button>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
