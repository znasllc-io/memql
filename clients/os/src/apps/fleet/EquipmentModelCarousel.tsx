import { Box } from "lucide-react";
import { CardCarousel } from "../../kit/CardCarousel";
import { modelPurposeLabel, type MachineModel } from "./machines/models";

/** The installed models, for display: the arrows page through them and a
 *  model is not a control. Models opens one for its details. */
export function EquipmentModelCarousel({ models }: { models: MachineModel[] }) {
  return <CardCarousel items={models} itemKey={model => model.modelId} label="Local models" noun="model"
    renderItem={model => <div className="fleet-equipment-slot">
      <Box size={17} aria-hidden />
      <span><strong>{model.modelId}</strong>
        <small>{modelPurposeLabel(model)}</small>
      </span>
    </div>} />;
}
