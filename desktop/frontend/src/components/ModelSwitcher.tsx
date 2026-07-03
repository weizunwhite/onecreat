import { useEffect, useRef, useState } from "react";
import { Check, ChevronsUpDown, Coins } from "lucide-react";
import { app } from "../lib/bridge";
import { alertDialog } from "../lib/confirm";
import { useT } from "../lib/i18n";
import { useSession, setSessionStore } from "../lib/account";
import type { ModelInfo } from "../lib/types";

// ModelSwitcher is the bottom-of-window picker. Two modes:
//  - 本地 API 模式(未登录):列出 config 里的模型,切换模型。
//  - 平台网关模式(已登录且平台配了档位):显示「档位」(标准/高级/旗舰)+ 剩余点数,
//    用户不知道背后是什么模型;切换调 SetOnecreatTier,由平台把档位映射到模型。
export function ModelSwitcher({ label, onPick }: { label: string; onPick: (name: string) => void }) {
  const t = useT();
  const session = useSession();
  const [open, setOpen] = useState(false);
  const [models, setModels] = useState<ModelInfo[]>([]);

  // 只有平台账号登录后才走网关(API 由平台统一分配),与 SettingsPanel 的 gatewayMode 谓词保持一致。
  const gatewayMode = !!session?.loggedIn;
  const tierMode = gatewayMode && (session?.tiers?.length ?? 0) > 0;

  useEffect(() => {
    // 仅未登录(直连)模式才列出 config 模型;网关模式下绝不拉真实模型名(M2:防泄露 + 防误切)。
    if (open && !gatewayMode) app.Models().then(setModels).catch(() => {});
  }, [open, gatewayMode]);

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
      const prevTier = sel;
      setSessionStore({ ...session, selectedTier: index }); // 乐观更新
      app.SetOnecreatTier(index).catch((e) => {
        // 后端拒绝(有标签正在跑任务,切档会重建其 controller 丢在途回合):回滚乐观更新并提示。
        setSessionStore({ ...session, selectedTier: prevTier });
        void alertDialog(String((e as { message?: string })?.message ?? e));
      });
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

  // —— 网关模式但平台未配档位(超管 / 未配机构):AI 仍走网关,绝不回退到真实模型选择器。
  //    此前这种情况 tierMode=false 会落到下面普通模式,露出 app.Models() 的真实模型名并允许
  //    SetModel(M2/L3)。这里只显示点数 / 不限,不可展开、不泄露模型。
  if (gatewayMode && session) {
    return (
      <div className="modelsw">
        <button className="modelsw__trigger" title="AI 由平台统一分配" disabled>
          <span className="modelsw__label">智能</span>
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
        </button>
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
